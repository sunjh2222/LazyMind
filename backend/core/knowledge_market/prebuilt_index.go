package knowledge_market

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lazymind/core/asyncjob"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/doc"
	"lazymind/core/log"
	"lazymind/core/store"
)

var errPrebuiltIndexWaitingForText = errors.New("waiting for prebuilt text import to finish")
var errPrebuiltIndexAlreadyStarted = errors.New("prebuilt index dispatch has already started for this job")
var errPrebuiltIndexInstallLeaseLost = errors.New("prebuilt index install lease was lost before dispatch could be registered")

type prebuiltIndexJobPayload struct {
	InstallJobID string                       `json:"install_job_id"`
	DatasetID    string                       `json:"dataset_id"`
	Release      prebuiltRelease              `json:"release"`
	Confirmation installIndexConfirmation     `json:"confirmation"`
	TaskIDs      []string                     `json:"task_ids"`
	RetryOf      string                       `json:"retry_of,omitempty"`
	RetryTargets []doc.MarketIndexRetryTarget `json:"retry_targets,omitempty"`
}

func prebuiltIndexJobKey(installJobID string) string { return "kb_index:" + installJobID }

func completePrebuiltMarketInstall(ctx context.Context, db *gorm.DB, job asyncjob.Job, payload installJobPayload, cfg installConfig,
	result *doc.MarketImportResult) (asyncjob.Result, error) {
	out := prebuiltImportResult(result)
	if payload.ProcessingLevel != doc.ProcessingLevelIndexed || payload.IndexConfirmation == nil ||
		payload.Prebuilt == nil || cfg.Prebuilt == nil || *payload.Prebuilt != *cfg.Prebuilt ||
		len(cfg.TaskIDs) != payload.Prebuilt.DocumentCount || len(cfg.Failures) != 0 {
		return out, nil
	}
	err := common.ImmediateTransactionWithSQLiteBusyRetry(ctx, db, func(tx *gorm.DB) error {
		// Lock the original installation before writing its child, in the
		// same order as deleting install history and canceling its child.
		var parent orm.AsyncJob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("status", "attempt_count", "lock_until").
			Take(&parent, "id = ? AND job_type = ? AND resource_id = ? AND create_user_id = ?",
				job.ID, installJobType, payload.MarketItemID, payload.UserID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errPrebuiltIndexInstallLeaseLost
		}
		if err != nil {
			return err
		}
		if parent.Status != string(asyncjob.StatusRunning) || job.AttemptCount < 1 || parent.AttemptCount != job.AttemptCount ||
			parent.LockUntil == nil || !parent.LockUntil.After(time.Now()) {
			return errPrebuiltIndexInstallLeaseLost
		}
		if _, err := asyncjob.EnqueueInTransaction(ctx, tx, asyncjob.EnqueueRequest{
			JobType: doc.MarketIndexJobType, ResourceType: "knowledge_market_item", ResourceID: payload.MarketItemID,
			CreateUserID: payload.UserID, CreateUserName: payload.UserName, MaxAttempts: 1,
			IdempotencyKey: prebuiltIndexJobKey(job.ID), Payload: prebuiltIndexJobPayload{
				InstallJobID: job.ID, DatasetID: result.DatasetID, Release: *payload.Prebuilt,
				Confirmation: *payload.IndexConfirmation, TaskIDs: cfg.TaskIDs,
			},
		}); err != nil {
			return err
		}
		if !parent.LockUntil.After(time.Now()) {
			return errPrebuiltIndexInstallLeaseLost
		}
		return nil
	})
	return out, err
}

func handlePrebuiltIndexJob(ctx context.Context, job asyncjob.Job, _ asyncjob.Reporter) (asyncjob.Result, error) {
	var indexJob prebuiltIndexJobPayload
	if err := json.Unmarshal(job.PayloadJSON, &indexJob); err != nil || indexJob.InstallJobID == "" || indexJob.DatasetID == "" ||
		indexJob.Release.URL == "" || !validPrebuiltRelease(&indexJob.Release, job.ResourceID) || !indexJob.Confirmation.valid() ||
		len(indexJob.TaskIDs) != indexJob.Release.DocumentCount || indexJob.RetryOf == job.ID ||
		(indexJob.RetryOf == "") != (len(indexJob.RetryTargets) == 0) {
		return asyncjob.Result{Permanent: true}, errors.New("invalid prebuilt indexing job")
	}
	db := store.DB()
	if db == nil {
		return asyncjob.Result{}, errors.New("store not initialized")
	}
	var parent orm.AsyncJob
	err := db.WithContext(ctx).Select("status").Take(&parent, "id = ? AND job_type = ? AND resource_id = ? AND create_user_id = ?",
		indexJob.InstallJobID, installJobType, job.ResourceID, job.CreateUserID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return asyncjob.Result{}, err
	}
	// Successful install history may be deleted while this job is queued.
	// The durable dispatch inputs stay in this job and match the installed copy.
	if err == nil {
		switch asyncjob.Status(parent.Status) {
		case asyncjob.StatusRunning, asyncjob.StatusPending:
			return waitForPrebuiltIndexText()
		case asyncjob.StatusSucceeded:
		default:
			return asyncjob.Result{}, nil
		}
	}
	installed, err := loadMarketInstallRow(ctx, db, job.CreateUserID, job.ResourceID)
	if err != nil {
		return asyncjob.Result{}, err
	}
	cfg := decodeInstallConfig(installed)
	if cfg.IndexDispatchJobID == job.ID || (indexJob.RetryOf != "" && cfg.IndexDispatchJobID != indexJob.RetryOf) ||
		!matchesPrebuiltIndexInstall(installed, cfg, indexJob.DatasetID, indexJob.Release, indexJob.TaskIDs) {
		return asyncjob.Result{}, nil
	}

	imports, waiting, err := prebuiltIndexImports(ctx, db, indexJob.DatasetID, indexJob.TaskIDs)
	if err != nil {
		return asyncjob.Result{}, err
	}
	if waiting {
		return waitForPrebuiltIndexText()
	}
	if len(imports) == 0 {
		return asyncjob.Result{}, nil
	}

	confirmation := indexJob.Confirmation
	currentModel, _, err := currentInstallIndexModel(ctx, db, job.CreateUserID)
	if err != nil {
		return asyncjob.Result{}, errors.New("could not verify the confirmed prebuilt indexing model")
	}
	if currentModel == nil || *currentModel != confirmation {
		log.Logger.Info().Str("market_item_id", job.ResourceID).Msg("skipped prebuilt index dispatch because the model changed")
		return asyncjob.Result{}, nil
	}
	identity := doc.MarketEmbeddingIdentity{Source: confirmation.Source, ModelID: confirmation.ModelID,
		Signature: confirmation.Signature, Generation: confirmation.Generation}
	payload := installJobPayload{MarketItemID: job.ResourceID, UserID: job.CreateUserID, Prebuilt: &indexJob.Release}
	err = doc.SubmitPrebuiltMarketReembed(ctx, db, indexJob.DatasetID, job.CreateUserID, job.CreateUserName, imports, identity, indexJob.InstallJobID, indexJob.RetryTargets,
		func(ctx context.Context) error {
			if indexJob.RetryOf != "" {
				return claimPrebuiltIndexDispatchAfter(ctx, db, job, payload, indexJob.DatasetID, indexJob.TaskIDs, indexJob.RetryOf)
			}
			return claimPrebuiltIndexDispatch(ctx, db, job, payload, indexJob.DatasetID, indexJob.TaskIDs)
		},
		func(ctx context.Context, currentDB *gorm.DB) error {
			installed, err := loadMarketInstallRow(ctx, currentDB, job.CreateUserID, job.ResourceID)
			if err != nil {
				return err
			}
			cfg := decodeInstallConfig(installed)
			if cfg.IndexDispatchJobID != job.ID || !matchesPrebuiltIndexInstall(installed, cfg, indexJob.DatasetID, indexJob.Release, indexJob.TaskIDs) {
				return errStaleInstallConfirmation
			}
			return requirePrebuiltIndexDispatchLease(currentDB.WithContext(ctx), job, job.ResourceID, job.CreateUserID)
		})
	if errors.Is(err, errStaleInstallConfirmation) || errors.Is(err, doc.ErrMarketEmbeddingChanged) {
		log.Logger.Info().Str("market_item_id", payload.MarketItemID).Msg("skipped prebuilt index dispatch because confirmation changed")
		return asyncjob.Result{}, nil
	}
	if errors.Is(err, errPrebuiltIndexAlreadyStarted) {
		return asyncjob.Result{}, nil
	}
	return asyncjob.Result{Permanent: err != nil}, err
}

func waitForPrebuiltIndexText() (asyncjob.Result, error) {
	// Poll the original import receipts without holding a worker slot or
	// repeating an external submission.
	return asyncjob.Result{DeferAttempt: true, RetryAt: time.Now().Add(time.Second)}, errPrebuiltIndexWaitingForText
}

func prebuiltIndexImports(ctx context.Context, db *gorm.DB, datasetID string, taskIDs []string) ([]orm.Task, bool, error) {
	var dataset orm.Dataset
	if err := db.WithContext(ctx).Select("kb_id").Take(&dataset, "id = ? AND deleted_at IS NULL", datasetID).Error; err != nil {
		return nil, false, err
	}
	var bindings []struct {
		orm.Task
		RemoteDocID string
	}
	if err := db.WithContext(ctx).Table("tasks AS t").Select("t.*, d.lazyllm_doc_id AS remote_doc_id").
		Joins("JOIN documents AS d ON d.id = t.doc_id AND d.dataset_id = t.dataset_id AND d.deleted_at IS NULL").
		Where("t.dataset_id = ? AND t.id IN ? AND t.task_type = ? AND t.deleted_at IS NULL", datasetID, taskIDs, doc.TaskTypeParseUploaded).
		Scan(&bindings).Error; err != nil {
		return nil, false, err
	}
	if dataset.KbID == "" || len(bindings) != len(taskIDs) {
		return nil, false, errors.New("the prebuilt text import bindings are incomplete")
	}
	remoteTasks := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.LazyllmTaskID == "" || binding.RemoteDocID == "" {
			return nil, false, errors.New("the prebuilt text import has no DocServer binding")
		}
		remoteTasks = append(remoteTasks, binding.LazyllmTaskID)
	}
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		return nil, false, errors.New("the prebuilt text receipt store is unavailable")
	}
	var receipts []readonlyorm.LazyLLMDocServiceTaskRow
	if err := lazyDB.WithContext(ctx).Where("kb_id = ? AND task_type = 'DOC_ADD' AND task_id IN ?", dataset.KbID, remoteTasks).
		Find(&receipts).Error; err != nil {
		return nil, false, err
	}
	if len(receipts) != len(bindings) {
		return nil, false, errors.New("the prebuilt text import receipts are incomplete")
	}
	byTask := make(map[string]readonlyorm.LazyLLMDocServiceTaskRow, len(receipts))
	for _, receipt := range receipts {
		byTask[receipt.TaskID] = receipt
	}
	imports := make([]orm.Task, 0, len(bindings))
	waiting := false
	for _, binding := range bindings {
		receipt, exists := byTask[binding.LazyllmTaskID]
		if !exists || receipt.DocID != binding.RemoteDocID {
			return nil, false, errors.New("the prebuilt text import receipt does not match its document")
		}
		switch doc.NormalizeTaskStateForUI(receipt.Status) {
		case "WAITING", "WORKING":
			waiting = true
		case "SUCCESS":
			if (receipt.ErrorCode != nil && strings.TrimSpace(*receipt.ErrorCode) != "" && *receipt.ErrorCode != "200") ||
				(receipt.ErrorMsg != nil && strings.TrimSpace(*receipt.ErrorMsg) != "" &&
					!strings.EqualFold(strings.TrimSpace(*receipt.ErrorMsg), "success")) {
				return nil, false, nil
			}
			imports = append(imports, binding.Task)
		default:
			return nil, false, nil
		}
	}
	return imports, waiting, nil
}

func matchesPrebuiltIndexInstall(installed *orm.KnowledgeMarketInstall, cfg installConfig, datasetID string, release prebuiltRelease, taskIDs []string) bool {
	return installed != nil && installed.DatasetID == datasetID && installed.InstallState == string(orm.InstallStateDone) &&
		installed.InstalledVersion == release.Version && cfg.Prebuilt != nil && *cfg.Prebuilt == release &&
		len(cfg.Failures) == 0 && slices.Equal(cfg.TaskIDs, taskIDs)
}

func requirePrebuiltIndexDispatchLease(db *gorm.DB, job asyncjob.Job, itemID, userID string) error {
	var active orm.AsyncJob
	if err := db.Select("lock_until").Take(&active,
		"id = ? AND resource_id = ? AND create_user_id = ? AND status = ? AND attempt_count = ?",
		job.ID, itemID, userID, asyncjob.StatusRunning, job.AttemptCount).Error; err != nil {
		return err
	}
	if active.LockUntil == nil || !active.LockUntil.After(time.Now()) {
		return errors.New("prebuilt index dispatch job lease expired")
	}
	return nil
}

func claimPrebuiltIndexDispatch(ctx context.Context, db *gorm.DB, job asyncjob.Job, payload installJobPayload, datasetID string, taskIDs []string) error {
	return claimPrebuiltIndexDispatchAfter(ctx, db, job, payload, datasetID, taskIDs, "")
}

func claimPrebuiltIndexDispatchAfter(ctx context.Context, db *gorm.DB, job asyncjob.Job, payload installJobPayload,
	datasetID string, taskIDs []string, previousJobID string) error {
	var item orm.KnowledgeMarketItem
	if err := db.WithContext(ctx).Take(&item, "id = ? AND status = ?", payload.MarketItemID, "published").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errStaleInstallConfirmation
		}
		return err
	}
	if prebuiltReleaseURL(item) != payload.Prebuilt.URL {
		return errStaleInstallConfirmation
	}
	releaseCtx, cancel := context.WithTimeout(ctx, installPreviewTimeout)
	defer cancel()
	latest, err := readVerifiedInstallRelease(releaseCtx, item)
	if err != nil {
		return errors.New("could not verify current prebuilt knowledge release for indexing")
	}
	if *latest != *payload.Prebuilt {
		return errStaleInstallConfirmation
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var currentItem orm.KnowledgeMarketItem
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Take(&currentItem, "id = ? AND status = ?", item.ID, "published").Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errStaleInstallConfirmation
			}
			return err
		}
		if prebuiltReleaseURL(currentItem) != latest.URL {
			return errStaleInstallConfirmation
		}
		installed, err := doc.LockMarketInstall(ctx, tx, payload.UserID, item.ID)
		if err != nil {
			return err
		}
		cfg := decodeInstallConfig(installed)
		if !matchesPrebuiltIndexInstall(installed, cfg, datasetID, *latest, taskIDs) {
			return errStaleInstallConfirmation
		}
		if cfg.IndexDispatchJobID == job.ID {
			return errPrebuiltIndexAlreadyStarted
		}
		if previousJobID != "" && cfg.IndexDispatchJobID != previousJobID {
			return errStaleInstallConfirmation
		}
		if err := requirePrebuiltIndexDispatchLease(tx.Clauses(clause.Locking{Strength: "UPDATE"}), job, item.ID, payload.UserID); err != nil {
			return err
		}
		// Reserve the whole dispatch once before sending any batch. A new
		// attempt must not replay a batch whose acceptance may be unknown.
		cfg.IndexDispatchJobID = job.ID
		data, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		return tx.Model(installed).UpdateColumn("config", json.RawMessage(data)).Error
	})
}
