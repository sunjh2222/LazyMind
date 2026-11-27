package knowledge_market

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"lazymind/core/asyncjob"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
	"lazymind/core/systemdeps"
)

func handlePrebuiltInstall(ctx context.Context, db *gorm.DB, job asyncjob.Job, payload installJobPayload, item orm.KnowledgeMarketItem,
	releaseURL string, previous *orm.KnowledgeMarketInstall, reporter asyncjob.Reporter) (asyncjob.Result, error) {
	cfg := decodeInstallConfig(previous)
	datasetID := ""
	if previous != nil {
		datasetID = previous.DatasetID
	}
	if datasetID != "" && cfg.Prebuilt == nil {
		return asyncjob.Result{Permanent: true}, fmt.Errorf("installed original-source dataset cannot be replaced by a prebuilt install")
	}
	if payload.Prebuilt != nil {
		if cfg.Prebuilt != nil && *cfg.Prebuilt != *payload.Prebuilt {
			return asyncjob.Result{Permanent: true}, errMarketSourceChanged
		}
		cfg.Prebuilt = payload.Prebuilt
	}
	if datasetID != "" && len(payload.PrebuiltRetryTasks) != 0 {
		result := &doc.MarketImportResult{
			DatasetID: datasetID, TaskIDs: cfg.TaskIDs, Failures: cfg.Failures,
			DispatchedTaskIDs: payload.PrebuiltRetryTasks,
		}
		if err := doc.MarketImportCheckpoint(ctx, result); err != nil {
			return asyncjob.Result{}, err
		}
		var err error
		result.Submitted, err = doc.RetryPrebuiltMarketTasks(ctx, datasetID, payload.UserID, payload.UserName, payload.PrebuiltRetryAttempt, payload.PrebuiltRetryTasks)
		if err != nil {
			return asyncjob.Result{ErrorCode: asyncjob.ErrorCodeHandlerFailed}, err
		}
		if err := resetInstallStateOnly(ctx, db, item.ID, payload.UserID, orm.InstallStateDone); err != nil {
			return asyncjob.Result{}, err
		}
		return prebuiltImportResult(result), nil
	}
	if datasetID != "" && len(cfg.TaskIDs) > 0 {
		if payload.IndexConfirmation != nil && previous.InstallState == string(orm.InstallStateDone) {
			result := &doc.MarketImportResult{DatasetID: datasetID, TaskIDs: cfg.TaskIDs, Failures: cfg.Failures}
			return completePrebuiltMarketInstall(ctx, db, job, payload, cfg, result)
		}
		parse := parseProgress((&http.Request{}).WithContext(ctx), db, previous)
		if parse.State == "done" || parseIsActive(parse) {
			return prebuiltImportResult(&doc.MarketImportResult{DatasetID: datasetID, TaskIDs: cfg.TaskIDs, Failures: cfg.Failures}), nil
		}
	}
	fail := func(err error) (asyncjob.Result, error) {
		if ctx.Err() != nil {
			return asyncjob.Result{ErrorCode: asyncjob.ErrorCodeCanceled}, ctx.Err()
		}
		if stateErr := setInstallState(ctx, db, item.ID, payload.UserID, orm.InstallStateFailed, datasetID, "", &cfg); stateErr != nil {
			return asyncjob.Result{ErrorCode: asyncjob.ErrorCodeHandlerFailed}, fmt.Errorf("%w (save install failure: %v)", err, stateErr)
		}
		return asyncjob.Result{ErrorCode: asyncjob.ErrorCodeHandlerFailed}, err
	}
	if cfg.Prebuilt == nil {
		release, err := readPrebuiltRelease(ctx, releaseURL, item.ID)
		if err != nil {
			return fail(err)
		}
		cfg.Prebuilt = release
	}
	if cfg.CandidateDatasetID == "" {
		identity := sha256.Sum256([]byte(job.ID + "\x00" + payload.UserID + "\x00" + item.ID))
		cfg.CandidateDatasetID = fmt.Sprintf("ds_%x", identity[:16])
	}
	if err := setInstallState(ctx, db, item.ID, payload.UserID, orm.InstallStateDownloading, datasetID, "", &cfg); err != nil {
		return asyncjob.Result{}, err
	}
	if reporter != nil {
		_ = reporter.SetProgress(ctx, 0, 2)
	}
	tmpDir := filepath.Join(doc.UploadRoot(), "market", "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fail(err)
	}
	root, err := os.MkdirTemp(tmpDir, "prebuilt-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(root)
	files, err := fetchPrebuiltFiles(ctx, cfg.Prebuilt, root)
	if err != nil {
		return fail(err)
	}
	if !systemdeps.PythonComponentActive("rag") {
		return fail(fmt.Errorf("prebuilt text import requires the RAG component"))
	}
	if reporter != nil {
		_ = reporter.SetProgress(ctx, 1, 2)
	}

	dataset, err := doc.CreatePrebuiltMarketDataset(ctx, payload.UserID, payload.UserName, cfg.CandidateDatasetID, item.Name, item.Description, tagsFromItem(item))
	if err != nil {
		return fail(fmt.Errorf("create dataset failed: %w", err))
	}
	datasetID = dataset.ID
	if err := setInstallState(ctx, db, item.ID, payload.UserID, orm.InstallStateImporting, datasetID, "", &cfg); err != nil {
		return fail(err)
	}
	result, err := doc.ImportMarketFiles(ctx, dataset, payload.UserID, payload.UserName, files)
	if result != nil {
		cfg.TaskIDs = result.TaskIDs
		cfg.Failures = result.Failures
	}
	if err != nil {
		return fail(fmt.Errorf("import prebuilt documents: %w", err))
	}
	// The install APIs derive text readiness from the authoritative worker
	// task states. Completion of the submission alone is not readiness.
	if err := setInstallState(ctx, db, item.ID, payload.UserID, orm.InstallStateDone, datasetID, cfg.Prebuilt.Version, &cfg); err != nil {
		return asyncjob.Result{}, err
	}
	if reporter != nil {
		_ = reporter.SetProgress(ctx, 2, 2)
	}
	return completePrebuiltMarketInstall(ctx, db, job, payload, cfg, result)
}

func prebuiltImportResult(result *doc.MarketImportResult) asyncjob.Result {
	raw, _ := json.Marshal(result)
	return asyncjob.Result{ResultJSON: raw}
}

func recoverablePrebuiltRetry(ctx context.Context, db *gorm.DB, job orm.AsyncJob, install *orm.KnowledgeMarketInstall, parse parseProgressInfo) (installJobPayload, bool) {
	if job.JobType != installJobType || job.Status != string(asyncjob.StatusFailed) ||
		job.FinishedAt == nil || job.MaxAttempts < 1 || job.AttemptCount < job.MaxAttempts ||
		job.LockedBy != "" || job.LockUntil != nil || parse.State != "unknown" {
		return installJobPayload{}, false
	}
	if install == nil || install.DatasetID == "" || install.UserID != job.CreateUserID || install.MarketItemID != job.ResourceID {
		return installJobPayload{}, false
	}
	cfg := decodeInstallConfig(install)
	payload, err := decodeInstallPayload(job.PayloadJSON)
	if err != nil || payload.MarketItemID != job.ResourceID || payload.UserID != job.CreateUserID ||
		cfg.Prebuilt == nil || payload.Prebuilt == nil || *cfg.Prebuilt != *payload.Prebuilt ||
		payload.OriginalSource || payload.PrebuiltRetryAttempt == "" || len(payload.PrebuiltRetryTasks) == 0 {
		return installJobPayload{}, false
	}
	var result struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(job.ResultJSON, &result) != nil || result.DatasetID != install.DatasetID {
		return installJobPayload{}, false
	}
	for _, id := range payload.PrebuiltRetryTasks {
		if !slices.Contains(cfg.TaskIDs, id) {
			return installJobPayload{}, false
		}
	}
	observedUnknown := 0
	for _, file := range parse.Files {
		if file.ReasonCode != "unknown" {
			continue
		}
		if !slices.Contains(payload.PrebuiltRetryTasks, file.FileID) {
			return installJobPayload{}, false
		}
		observedUnknown++
	}
	if observedUnknown == 0 || observedUnknown != parse.Unknown ||
		!doc.HasUnconfirmedPrebuiltRetry(ctx, db, install.DatasetID, payload.PrebuiltRetryAttempt, payload.PrebuiltRetryTasks) {
		return installJobPayload{}, false
	}
	return payload, true
}

// A new user retry starts a fresh submission only after a known failure.
// When a previous retry is still unconfirmed, keep its frozen targets and
// attempt ID so a lost response can be recovered with the same request.
func planPrebuiltRetry(payload *installJobPayload, parse parseProgressInfo) {
	if parseIsActive(parse) || parse.Failed == 0 || !canRetryMarketParse(parse) {
		return
	}
	payload.PrebuiltRetryTasks = nil
	for _, failure := range parse.Failures {
		payload.PrebuiltRetryTasks = append(payload.PrebuiltRetryTasks, failure.TaskID)
	}
	if len(payload.PrebuiltRetryTasks) != 0 {
		payload.PrebuiltRetryAttempt = uuid.NewString()
	}
}
