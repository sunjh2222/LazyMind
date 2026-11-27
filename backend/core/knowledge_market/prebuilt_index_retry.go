package knowledge_market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"lazymind/core/asyncjob"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
)

var (
	errPrebuiltIndexNotRetryable      = errors.New("no confirmed prebuilt index failure to retry")
	errPrebuiltIndexReleaseUnchecked  = errors.New("could not verify current prebuilt knowledge release for indexing")
	errPrebuiltIndexWorkerUnavailable = errors.New("prebuilt indexing worker is unavailable")
)

func retryFailedPrebuiltIndex(w http.ResponseWriter, r *http.Request, db *gorm.DB, parent orm.AsyncJob, installed *orm.KnowledgeMarketInstall) {
	job, err := enqueuePrebuiltIndexRetry(r.Context(), db, parent, installed, common.UserName(r))
	switch {
	case errors.Is(err, errPrebuiltIndexNotRetryable):
		common.ReplyAppErr(w, common.NewAppError(http.StatusConflict, common.ErrCodeConflict, "Only failed tasks can be retried"))
	case errors.Is(err, errStaleInstallConfirmation):
		common.ReplyErr(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errPrebuiltIndexReleaseUnchecked):
		common.ReplyErr(w, err.Error(), http.StatusBadGateway)
	case errors.Is(err, errPrebuiltIndexWorkerUnavailable):
		marketUnavailable(w)
	case err != nil:
		replyServiceError(w, err)
	default:
		common.ReplyOK(w, map[string]any{"job_id": job.ID, "state": job.Status, "parent_job_id": parent.ID})
	}
}

func enqueuePrebuiltIndexRetry(ctx context.Context, db *gorm.DB, parent orm.AsyncJob, installed *orm.KnowledgeMarketInstall, userName string) (*orm.AsyncJob, error) {
	source, payload, err := loadPrebuiltIndexRetrySource(db.WithContext(ctx), parent, installed)
	if err != nil {
		return nil, err
	}
	imports, waiting, err := prebuiltIndexImports(ctx, db, payload.DatasetID, payload.TaskIDs)
	if err != nil || waiting || len(imports) != len(payload.TaskIDs) {
		return nil, errPrebuiltIndexNotRetryable
	}
	// Recovery binds results already accepted for these installed document
	// versions. Only a fresh submission needs a live upstream/model check.
	recovered, err := doc.RecoverPrebuiltMarketIndexReceipts(ctx, db, payload.DatasetID, parent.ID, imports)
	if err != nil {
		return nil, err
	}
	targets, err := doc.PrebuiltMarketIndexRetryTargets(ctx, db, payload.DatasetID, parent.ID, imports)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		if recovered > 0 {
			return &parent, nil
		}
		return nil, errPrebuiltIndexNotRetryable
	}
	if marketWorkerHealth(ctx) != "available" {
		return nil, errPrebuiltIndexWorkerUnavailable
	}
	var item orm.KnowledgeMarketItem
	if err := db.WithContext(ctx).Take(&item, "id = ? AND status = ?", parent.ResourceID, "published").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errStaleInstallConfirmation
		}
		return nil, err
	}
	if prebuiltReleaseURL(item) != payload.Release.URL {
		return nil, errStaleInstallConfirmation
	}
	verifyCtx, cancel := context.WithTimeout(ctx, installPreviewTimeout)
	defer cancel()
	latest, err := readVerifiedInstallRelease(verifyCtx, item)
	if err != nil {
		return nil, errPrebuiltIndexReleaseUnchecked
	}
	if *latest != payload.Release {
		return nil, errStaleInstallConfirmation
	}
	model, _, err := currentInstallIndexModel(verifyCtx, db, parent.CreateUserID)
	if err != nil || model == nil || *model != payload.Confirmation {
		return nil, errStaleInstallConfirmation
	}
	payload.RetryOf, payload.RetryTargets = source.ID, targets
	var job *orm.AsyncJob
	err = common.ImmediateTransactionWithSQLiteBusyRetry(ctx, db, func(tx *gorm.DB) error {
		var currentItem orm.KnowledgeMarketItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&currentItem, "id = ? AND status = ?", parent.ResourceID, "published").Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errStaleInstallConfirmation
			}
			return err
		}
		if prebuiltReleaseURL(currentItem) != payload.Release.URL {
			return errStaleInstallConfirmation
		}
		current, err := doc.LockMarketInstall(ctx, tx, parent.CreateUserID, parent.ResourceID)
		if err != nil {
			return err
		}
		if decodeInstallConfig(current).IndexDispatchJobID != source.ID {
			return errPrebuiltIndexNotRetryable
		}
		var currentParent orm.AsyncJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&currentParent, "id = ? AND job_type = ? AND resource_id = ? AND create_user_id = ?",
			parent.ID, installJobType, parent.ResourceID, parent.CreateUserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errPrebuiltIndexNotRetryable
			}
			return err
		}
		// A no-op update adds history without changing the installed release or
		// the index dispatch. Check those bindings instead of history order.
		currentSource, _, err := loadPrebuiltIndexRetrySource(tx.Clauses(clause.Locking{Strength: "UPDATE"}), currentParent, current)
		if err != nil {
			return err
		}
		if !bytes.Equal(currentSource.PayloadJSON, source.PayloadJSON) {
			return errPrebuiltIndexNotRetryable
		}
		active, err := doc.HasActiveMarketJob(ctx, tx, parent.CreateUserID, parent.ResourceID)
		if err != nil {
			return err
		}
		if active {
			return errMarketBusy
		}
		if err := lockInstallIndexConfirmation(tx, &payload.Confirmation); err != nil {
			return err
		}
		// Keep the original child and its receipts as history. While waiting, the
		// new child is protected by this install's active-job and dispatch fences.
		job, err = asyncjob.EnqueueInTransaction(ctx, tx, asyncjob.EnqueueRequest{
			JobType: doc.MarketIndexJobType, ResourceType: parent.ResourceType, ResourceID: parent.ResourceID,
			CreateUserID: parent.CreateUserID, CreateUserName: userName, MaxAttempts: 1, Payload: payload,
		})
		return err
	})
	return job, err
}

func loadPrebuiltIndexRetrySource(db *gorm.DB, parent orm.AsyncJob, installed *orm.KnowledgeMarketInstall) (orm.AsyncJob, prebuiltIndexJobPayload, error) {
	var source orm.AsyncJob
	var payload prebuiltIndexJobPayload
	cfg := decodeInstallConfig(installed)
	if parent.Status != string(asyncjob.StatusSucceeded) || cfg.IndexDispatchJobID == "" {
		return source, payload, errPrebuiltIndexNotRetryable
	}
	if err := db.Take(&source, "id = ? AND job_type = ? AND resource_id = ? AND create_user_id = ?",
		cfg.IndexDispatchJobID, doc.MarketIndexJobType, parent.ResourceID, parent.CreateUserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return source, payload, errPrebuiltIndexNotRetryable
		}
		return source, payload, err
	}
	if (source.Status != string(asyncjob.StatusSucceeded) && source.Status != string(asyncjob.StatusFailed)) ||
		source.LockedBy != "" || source.LockUntil != nil ||
		json.Unmarshal(source.PayloadJSON, &payload) != nil || payload.InstallJobID != parent.ID ||
		!matchesPrebuiltIndexInstall(installed, cfg, payload.DatasetID, payload.Release, payload.TaskIDs) {
		return source, payload, errPrebuiltIndexNotRetryable
	}
	confirmed, err := decodeInstallPayload(parent.PayloadJSON)
	if err != nil || confirmed.UserID != parent.CreateUserID || confirmed.MarketItemID != parent.ResourceID ||
		confirmed.ProcessingLevel != doc.ProcessingLevelIndexed || confirmed.Prebuilt == nil || *confirmed.Prebuilt != payload.Release ||
		confirmed.IndexConfirmation == nil || *confirmed.IndexConfirmation != payload.Confirmation {
		return source, payload, errPrebuiltIndexNotRetryable
	}
	return source, payload, nil
}
