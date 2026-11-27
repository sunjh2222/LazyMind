package knowledge_market

import (
	"context"
	"encoding/json"
	"slices"

	"gorm.io/gorm"
	"lazymind/core/asyncjob"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
)

// InstallIndexProgress describes vector work independently of text installation.
type InstallIndexProgress struct {
	State   string `json:"state" enum:"waiting,running,succeeded,partial_failed,failed,unknown,canceled"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Pending int    `json:"pending"`
	Unknown int    `json:"unknown"`
}

type installIndexProgressView struct {
	progress InstallIndexProgress
	canRetry bool
}

func (view *installIndexProgressView) active() bool {
	return view != nil && (view.progress.State == "waiting" || view.progress.State == "running")
}

type marketIndexProgressParent struct {
	job       orm.AsyncJob
	confirmed installJobPayload
	installed *orm.KnowledgeMarketInstall
	config    installConfig
}

type marketIndexProgressChild struct {
	job     orm.AsyncJob
	payload prebuiltIndexJobPayload
}

func loadInstallIndexProgress(ctx context.Context, db *gorm.DB, userID string, jobs []orm.AsyncJob,
	installs map[string]*orm.KnowledgeMarketInstall) (map[string]*installIndexProgressView, error) {
	progress := make(map[string]*installIndexProgressView)
	var parents []marketIndexProgressParent
	for _, job := range jobs {
		if job.JobType != installJobType || job.CreateUserID != userID {
			continue
		}
		confirmed, err := decodeInstallPayload(job.PayloadJSON)
		if err != nil || confirmed.UserID != userID || confirmed.MarketItemID != job.ResourceID ||
			confirmed.ProcessingLevel != doc.ProcessingLevelIndexed || confirmed.IndexConfirmation == nil || !confirmed.IndexConfirmation.valid() ||
			confirmed.Prebuilt == nil || !validPrebuiltRelease(confirmed.Prebuilt, job.ResourceID) {
			continue
		}
		count := confirmed.Prebuilt.DocumentCount
		switch job.Status {
		case string(asyncjob.StatusPending), string(asyncjob.StatusRunning):
			progress[job.ID] = &installIndexProgressView{progress: InstallIndexProgress{State: "waiting", Total: count, Pending: count}}
		case string(asyncjob.StatusSucceeded):
			installed := installs[job.ResourceID]
			cfg := decodeInstallConfig(installed)
			if installed != nil && matchesPrebuiltIndexInstall(installed, cfg, installed.DatasetID, *confirmed.Prebuilt, cfg.TaskIDs) && len(cfg.TaskIDs) == count {
				parents = append(parents, marketIndexProgressParent{job, confirmed, installed, cfg})
			}
		}
	}
	if len(parents) == 0 {
		return progress, nil
	}

	var children []orm.AsyncJob
	for group := range slices.Chunk(parents, 300) {
		var ids, keys, items []string
		for _, parent := range group {
			if parent.config.IndexDispatchJobID != "" {
				ids = append(ids, parent.config.IndexDispatchJobID)
			}
			keys = append(keys, prebuiltIndexJobKey(parent.job.ID))
			items = append(items, parent.job.ResourceID)
		}
		var batch []orm.AsyncJob
		if err := db.WithContext(ctx).Where("job_type = ? AND create_user_id = ?", doc.MarketIndexJobType, userID).
			Where("(id IN ? OR idempotency_key IN ? OR (status IN ? AND resource_id IN ?))", ids, keys, []string{"pending", "running"}, items).
			Order("created_at DESC, id DESC").Find(&batch).Error; err != nil {
			return nil, err
		}
		children = append(children, batch...)
	}
	byParent := make(map[string][]marketIndexProgressChild)
	for _, job := range children {
		var payload prebuiltIndexJobPayload
		if json.Unmarshal(job.PayloadJSON, &payload) == nil && payload.InstallJobID != "" {
			byParent[payload.InstallJobID] = append(byParent[payload.InstallJobID], marketIndexProgressChild{job, payload})
		}
	}
	var scopes []doc.MarketIndexReadScope
	selected := make(map[string]marketIndexProgressChild)
	for _, parent := range parents {
		activeID := parent.config.IndexDispatchJobID
		for _, candidate := range byParent[parent.job.ID] {
			job, payload := candidate.job, candidate.payload
			if job.ResourceID != parent.job.ResourceID || payload.Confirmation != *parent.confirmed.IndexConfirmation ||
				!matchesPrebuiltIndexInstall(parent.installed, parent.config, payload.DatasetID, payload.Release, payload.TaskIDs) {
				continue
			}
			if activeID == "" && job.IdempotencyKey != prebuiltIndexJobKey(parent.job.ID) {
				continue
			}
			activeRetry := activeID != "" && payload.RetryOf == activeID && marketIndexChildActive(job)
			if activeID != "" && job.ID != activeID && !activeRetry {
				continue
			}
			selected[parent.job.ID] = candidate
			scopes = append(scopes, doc.MarketIndexReadScope{InstallJobID: parent.job.ID, DatasetID: payload.DatasetID, UserID: userID, ImportTaskIDs: payload.TaskIDs})
			break
		}
	}
	observations, err := doc.ObservePrebuiltMarketIndexes(ctx, db, scopes)
	for _, parent := range parents {
		if child, ok := selected[parent.job.ID]; ok {
			observed := observations[parent.job.ID]
			if err != nil {
				// An unavailable receipt store does not change whether text is readable.
				observed = doc.MarketIndexObservations{Unknown: len(child.payload.TaskIDs)}
			}
			view := summarizeInstallIndexProgress(parent, child, observed)
			progress[parent.job.ID] = &view
		}
	}
	return progress, nil
}

func marketIndexChildActive(child orm.AsyncJob) bool {
	return child.Status == string(asyncjob.StatusPending) || child.Status == string(asyncjob.StatusRunning)
}

func summarizeInstallIndexProgress(parent marketIndexProgressParent, child marketIndexProgressChild, observed doc.MarketIndexObservations) installIndexProgressView {
	result := InstallIndexProgress{Total: len(child.payload.TaskIDs), Done: observed.Done, Failed: observed.Failed, Pending: observed.Pending, Unknown: observed.Unknown}
	active := marketIndexChildActive(child.job)
	claimed := parent.config.IndexDispatchJobID == child.job.ID
	canRetry := claimed && !active && observed.Pending == 0 && child.job.LockedBy == "" && child.job.LockUntil == nil &&
		(child.job.Status == string(asyncjob.StatusSucceeded) || child.job.Status == string(asyncjob.StatusFailed)) &&
		(observed.Recoverable || observed.Failed > 0 || observed.Unsubmitted > 0)
	switch {
	case active:
		result.Pending += observed.Unsubmitted
		result.State = "waiting"
		if observed.Pending > 0 || child.job.Status == string(asyncjob.StatusRunning) && claimed {
			result.State = "running"
		}
	case observed.Pending > 0:
		result.Pending += observed.Unsubmitted
		result.State = "running"
	case result.Done == result.Total:
		result.State = "succeeded"
	case child.job.Status == string(asyncjob.StatusCanceled), observed.Canceled > 0:
		result.State = "canceled"
	case child.job.Status == string(asyncjob.StatusFailed):
		result.Failed += observed.Unsubmitted
		result.State = "failed"
		if result.Done > 0 {
			result.State = "partial_failed"
		} else if result.Failed == 0 {
			result.State = "unknown"
		}
	case result.Failed > 0:
		result.State = "failed"
		if result.Done > 0 {
			result.State = "partial_failed"
		}
	case result.Unknown > 0:
		result.State = "unknown"
	default:
		// The child can stop without sending any remaining objects if the
		// confirmed model or installed release changes during dispatch.
		result.State = "canceled"
	}
	return installIndexProgressView{progress: result, canRetry: canRetry}
}
