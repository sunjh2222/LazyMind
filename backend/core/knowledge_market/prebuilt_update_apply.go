package knowledge_market

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lazymind/core/asyncjob"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
)

func pinPrebuiltUpdate(w http.ResponseWriter, r *http.Request, db *gorm.DB, item *orm.KnowledgeMarketItem,
	install *orm.KnowledgeMarketInstall, installed *prebuiltRelease, payload *updateJobPayload) bool {
	if install.DatasetID == "" || install.InstallState != string(orm.InstallStateDone) ||
		install.InstalledVersion != installed.Version || !validPrebuiltRelease(installed, item.ID) {
		common.ReplyErr(w, "prebuilt installation is not ready for update", http.StatusConflict)
		return false
	}
	if !doc.RequireMarketDatasetPermission(w, r, install.DatasetID) {
		return false
	}
	if releaseURL := prebuiltReleaseURL(*item); releaseURL == "" || releaseURL != installed.URL {
		replyServiceError(w, errMarketSourceChanged)
		return false
	}
	if progress := parseProgress(r, db, install); progress.State != "done" || progress.Done != installed.DocumentCount {
		common.ReplyErr(w, "prebuilt documents are not ready for update", http.StatusConflict)
		return false
	}
	target, err := readPrebuiltUpdateTarget(r.Context(), installed)
	if err != nil {
		common.ReplyErr(w, "could not verify the prebuilt release for update", http.StatusBadGateway)
		return false
	}
	payload.PrebuiltFrom = installed
	payload.PrebuiltTarget = target
	payload.PrebuiltDatasetID = install.DatasetID
	return true
}

func prebuiltUpdateFailure(err error, permanent bool) (asyncjob.Result, error) {
	// Do not borrow the old installation's successful file states for a failed update.
	return asyncjob.Result{
		Permanent:  permanent,
		ErrorCode:  asyncjob.ErrorCodeHandlerFailed,
		ResultJSON: json.RawMessage(`{"updated":false,"parse":{"state":"failed"}}`),
	}, err
}

func handlePrebuiltUpdate(ctx context.Context, db *gorm.DB, job asyncjob.Job, reporter asyncjob.Reporter,
	payload updateJobPayload, installed *orm.KnowledgeMarketInstall) (asyncjob.Result, error) {
	cfg := decodeInstallConfig(installed)
	if installed.DatasetID != payload.PrebuiltDatasetID || cfg.Prebuilt == nil ||
		installed.InstalledVersion != cfg.Prebuilt.Version {
		return prebuiltUpdateFailure(errMarketSourceChanged, true)
	}
	if *cfg.Prebuilt == *payload.PrebuiltTarget {
		return noChangeUpdateResult()
	}
	if *cfg.Prebuilt != *payload.PrebuiltFrom {
		return prebuiltUpdateFailure(errMarketSourceChanged, true)
	}
	var item orm.KnowledgeMarketItem
	if err := db.WithContext(ctx).Where("id = ? AND status = ?", payload.MarketItemID, "published").Take(&item).Error; err != nil {
		return prebuiltUpdateFailure(err, errors.Is(err, gorm.ErrRecordNotFound))
	}
	if prebuiltReleaseURL(item) != cfg.Prebuilt.URL {
		return prebuiltUpdateFailure(errMarketSourceChanged, true)
	}
	var ds orm.Dataset
	if err := db.WithContext(ctx).Where("id = ? AND create_user_id = ? AND deleted_at IS NULL",
		installed.DatasetID, payload.UserID).Take(&ds).Error; err != nil {
		return prebuiltUpdateFailure(err, errors.Is(err, gorm.ErrRecordNotFound))
	}

	base, err := prebuiltReleaseBase(cfg.Prebuilt.URL)
	if err != nil {
		return prebuiltUpdateFailure(err, true)
	}
	before, err := readPrebuiltDocuments(ctx, base, cfg.Prebuilt)
	if err != nil {
		return prebuiltUpdateFailure(errors.New("could not verify the installed prebuilt manifest"), false)
	}
	after, err := readPrebuiltDocuments(ctx, base, payload.PrebuiltTarget)
	if err != nil {
		return prebuiltUpdateFailure(errors.New("could not verify the target prebuilt manifest"), false)
	}
	changed, err := changedPrebuiltDocuments(before, after)
	if err != nil {
		return prebuiltUpdateFailure(err, true)
	}
	if len(changed) == 0 && len(before) == len(after) {
		return noChangeUpdateResult()
	}
	tasks, err := prebuiltSourceTasks(installed.DatasetID, cfg, before)
	if err != nil {
		return prebuiltUpdateFailure(err, true)
	}

	var root string
	if len(changed) != 0 {
		tmp := filepath.Join(doc.UploadRoot(), "market", "tmp")
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			return prebuiltUpdateFailure(err, false)
		}
		root, err = os.MkdirTemp(tmp, "prebuilt-update-")
		if err != nil {
			return prebuiltUpdateFailure(err, false)
		}
		defer os.RemoveAll(root)
	}
	type documentUpdate struct {
		sourceID  string
		oldTaskID string
		current   orm.Document
		file      doc.MarketImportFile
		staged    *doc.StagedPrebuiltMarketUpdate
	}
	updates := make([]documentUpdate, 0, len(changed))
	remainingDecoded := int64(prebuiltTotalLimit)
	for i, change := range changed {
		document := change.document
		update := documentUpdate{sourceID: document.ID, oldTaskID: tasks[document.ID]}
		if update.oldTaskID != "" {
			current, err := loadPrebuiltUpdateDocument(ctx, db, ds.ID, update.oldTaskID)
			switch {
			case !change.wasPublished && errors.Is(err, gorm.ErrRecordNotFound):
				// A reappearing source can no longer reuse a local copy
				// the user deleted while it was withdrawn.
				update.oldTaskID = ""
			case err != nil:
				return prebuiltUpdateFailure(err, errors.Is(err, gorm.ErrRecordNotFound))
			case !change.wasPublished && doc.PrebuiltMarketObjectMatches(current, document.ObjectSHA256):
				continue
			default:
				update.current = current
			}
		}
		file, decoded, err := fetchPrebuiltFile(ctx, base, payload.PrebuiltTarget, document, root, fmt.Sprint(i), remainingDecoded)
		if err != nil {
			var networkError *url.Error
			if errors.As(err, &networkError) {
				err = fmt.Errorf("could not download changed prebuilt object %q", document.ID)
			}
			return prebuiltUpdateFailure(err, false)
		}
		update.file = file
		remainingDecoded -= decoded
		updates = append(updates, update)
	}

	// The market task UI interprets the two steps as phases, independent of
	// how many documents are in the update.
	if reporter != nil && len(updates) != 0 {
		_ = reporter.SetProgress(ctx, 1, 2)
	}
	for i := range updates {
		update := &updates[i]
		var staged *doc.StagedPrebuiltMarketUpdate
		var err error
		if update.oldTaskID == "" {
			staged, err = doc.StagePrebuiltMarketAddition(ctx, db, &ds, job.ID, update.file)
		} else {
			// A retry keeps the same candidate until its active binding or
			// target manifest changes.
			candidate := sha256.Sum256([]byte(ds.ID + "\x00" + update.current.ID + "\x00" + update.current.LazyllmDocID +
				"\x00" + payload.PrebuiltTarget.ManifestSHA256))
			staged, err = doc.StagePrebuiltMarketUpdate(ctx, db, &ds, update.current,
				fmt.Sprintf("prebuiltv_%x", candidate[:16]), job.ID, update.file)
		}
		if err != nil {
			return prebuiltUpdateFailure(err, errors.Is(err, doc.ErrPrebuiltUpdateWorkerFailed))
		}
		update.staged = staged
	}

	newTaskIDs := make([]string, 0, len(updates))
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := doc.LockMarketInstall(ctx, tx, payload.UserID, payload.MarketItemID)
		if err != nil {
			return err
		}
		currentCfg := decodeInstallConfig(row)
		if row == nil || row.DatasetID != ds.ID || row.InstalledVersion != cfg.Prebuilt.Version ||
			row.InstallState != installed.InstallState || currentCfg.Prebuilt == nil || *currentCfg.Prebuilt != *cfg.Prebuilt ||
			!slices.Equal(currentCfg.TaskIDs, cfg.TaskIDs) || !maps.Equal(currentCfg.PrebuiltDocumentTasks, cfg.PrebuiltDocumentTasks) {
			return errMarketSourceChanged
		}
		var active orm.AsyncJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND create_user_id = ? AND status = ? AND attempt_count = ?",
			job.ID, payload.UserID, string(asyncjob.StatusRunning), job.AttemptCount).Take(&active).Error; err != nil {
			return err
		}
		if active.LockUntil == nil || !active.LockUntil.After(time.Now()) {
			return errors.New("prebuilt update job lease expired")
		}
		var live orm.Dataset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND kb_id = ? AND tenant_id = ? AND create_user_id = ? AND deleted_at IS NULL",
			ds.ID, ds.KbID, ds.TenantID, payload.UserID).Take(&live).Error; err != nil {
			return err
		}
		for _, update := range updates {
			var newTaskID string
			var err error
			if update.oldTaskID == "" {
				newTaskID, err = doc.CommitPrebuiltMarketAddition(ctx, tx, &live, payload.UserID, payload.UserName, update.staged)
			} else {
				newTaskID, err = doc.CommitPrebuiltMarketUpdate(ctx, tx, &live, update.current, update.oldTaskID, update.staged)
			}
			if err != nil {
				return err
			}
			tasks[update.sourceID] = newTaskID
			newTaskIDs = append(newTaskIDs, newTaskID)
		}
		// Only tasks from the published release determine readiness. Keep
		// withdrawn bindings separately in the full source-to-task map.
		currentCfg.TaskIDs = make([]string, 0, len(after))
		for _, document := range after {
			currentCfg.TaskIDs = append(currentCfg.TaskIDs, tasks[document.ID])
		}
		currentCfg.PrebuiltDocumentTasks = tasks
		currentCfg.Prebuilt = payload.PrebuiltTarget
		return setInstallState(ctx, tx, payload.MarketItemID, payload.UserID,
			orm.InstallStateDone, ds.ID, payload.PrebuiltTarget.Version, &currentCfg)
	})
	if err != nil {
		return prebuiltUpdateFailure(err, errors.Is(err, errMarketSourceChanged))
	}
	if len(newTaskIDs) == 0 {
		return noChangeUpdateResult()
	}
	if reporter != nil {
		_ = reporter.SetProgress(ctx, 2, 2)
	}
	result, _ := json.Marshal(map[string]any{"updated": true, "dataset_id": ds.ID, "task_ids": newTaskIDs})
	return asyncjob.Result{ResultJSON: result}, nil
}

func loadPrebuiltUpdateDocument(ctx context.Context, db *gorm.DB, datasetID, taskID string) (orm.Document, error) {
	var document orm.Document
	var task orm.Task
	if err := db.WithContext(ctx).Where("id = ? AND dataset_id = ? AND deleted_at IS NULL",
		taskID, datasetID).Take(&task).Error; err != nil {
		return document, err
	}
	err := db.WithContext(ctx).Where("id = ? AND dataset_id = ? AND deleted_at IS NULL",
		task.DocID, datasetID).Take(&document).Error
	return document, err
}

type prebuiltDocumentChange struct {
	document     prebuiltDocument
	wasPublished bool
}

func changedPrebuiltDocuments(before, after []prebuiltDocument) ([]prebuiltDocumentChange, error) {
	byID := make(map[string]prebuiltDocument, len(before))
	for _, document := range before {
		byID[document.ID] = document
	}
	var changed []prebuiltDocumentChange
	for _, document := range after {
		previous, exists := byID[document.ID]
		if exists && previous.ObjectSHA256 == document.ObjectSHA256 {
			previous.SourceSHA256 = document.SourceSHA256
			if previous != document {
				return nil, errors.New("metadata changes to unchanged prebuilt objects are not supported")
			}
			continue
		}
		changed = append(changed, prebuiltDocumentChange{document: document, wasPublished: exists})
	}
	return changed, nil
}

func prebuiltSourceTasks(datasetID string, cfg installConfig, documents []prebuiltDocument) (map[string]string, error) {
	// The persisted map also tracks documents withdrawn from the upstream
	// manifest. Their local tasks and bindings remain live.
	tasks := maps.Clone(cfg.PrebuiltDocumentTasks)
	if tasks == nil {
		tasks = make(map[string]string, len(documents))
		for _, document := range documents {
			_, tasks[document.ID] = doc.PrebuiltMarketResourceIDs(datasetID, cfg.Prebuilt.ManifestSHA256+":"+document.ID)
		}
	}
	if len(cfg.TaskIDs) != len(documents) {
		return nil, errors.New("prebuilt installation is missing document tasks")
	}
	installed := make(map[string]bool, len(cfg.TaskIDs))
	for _, id := range cfg.TaskIDs {
		installed[id] = true
	}
	for _, document := range documents {
		id := tasks[document.ID]
		if id == "" || !installed[id] {
			return nil, errors.New("prebuilt installation does not match its manifest")
		}
		delete(installed, id)
	}
	seen := make(map[string]bool, len(tasks))
	for sourceID, id := range tasks {
		if sourceID == "" || id == "" || seen[id] {
			return nil, errors.New("prebuilt installation has an invalid document task mapping")
		}
		seen[id] = true
	}
	return tasks, nil
}
