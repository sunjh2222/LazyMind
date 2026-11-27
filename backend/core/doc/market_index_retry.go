package doc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

type marketIndexTaskOrigin struct {
	InstallJobID    string `json:"install_job_id"`
	ImportTaskID    string `json:"import_task_id"`
	DocumentVersion int64  `json:"document_version"`
}

// MarketIndexRetryTarget pairs the original document import and the explicitly failed index attempt.
type MarketIndexRetryTarget struct {
	ImportTaskID string `json:"import_task_id"`
	FailedTaskID string `json:"failed_task_id"`
	Unsubmitted  bool   `json:"unsubmitted,omitempty"`
}

// PrebuiltMarketIndexRetryTargets selects confirmed failures and documents that never crossed the send boundary.
func PrebuiltMarketIndexRetryTargets(ctx context.Context, db *gorm.DB, datasetID, installJobID string,
	imports []orm.Task) ([]MarketIndexRetryTarget, error) {
	if datasetID == "" || installJobID == "" || len(imports) == 0 {
		return nil, nil
	}
	lazyDB := store.LazyLLMDB()
	if db == nil || lazyDB == nil {
		return nil, errors.New("prebuilt market index receipt store is unavailable")
	}
	db, lazyDB = db.WithContext(ctx), lazyDB.WithContext(ctx)
	var dataset orm.Dataset
	if err := db.Take(&dataset, "id = ? AND deleted_at IS NULL", datasetID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if dataset.KbID == "" || dataset.ProcessingLevel != ProcessingLevelChunked || !isPrebuiltMarketDataset(dataset) {
		return nil, nil
	}
	var targets []MarketIndexRetryTarget
	for _, imported := range imports {
		if imported.ID == "" || imported.DocID == "" || imported.DatasetID != dataset.ID || imported.TaskType != string(TaskTypeParseUploaded) {
			continue
		}
		var document orm.Document
		if err := db.Take(&document, "id = ? AND dataset_id = ? AND deleted_at IS NULL", imported.DocID, dataset.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		var metadata documentExt
		if document.LazyllmDocID == "" || json.Unmarshal(document.Ext, &metadata) != nil || metadata.PrebuiltPath == "" {
			continue
		}
		var state orm.DocumentProcessingState
		if err := db.Take(&state, "dataset_id = ? AND document_id = ?", dataset.ID, document.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		task, err := latestPrebuiltMarketDocumentTask(db, dataset.ID, document.ID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		if state.IndexArtifactRef == "" && task.ID == imported.ID && state.IndexStatus != "running" && state.IndexStatus != "succeeded" {
			var history int64
			if err := db.Unscoped().Model(&orm.Task{}).Where("dataset_id = ? AND doc_id = ? AND task_type = ?",
				dataset.ID, document.ID, TaskTypeReparse).Count(&history).Error; err != nil {
				return nil, err
			}
			if history == 0 {
				targets = append(targets, MarketIndexRetryTarget{ImportTaskID: imported.ID, FailedTaskID: imported.ID, Unsubmitted: true})
			}
			continue
		}
		failedTaskID, bound := strings.CutPrefix(state.IndexArtifactRef, "core-task:")
		if !bound || failedTaskID == "" || task.ID != failedTaskID {
			continue
		}
		prepared := MarketIndexRetryTarget{ImportTaskID: imported.ID, FailedTaskID: task.ID, Unsubmitted: true}
		if matchesMarketIndexRetryTask(task, dataset, document, imported.ID, installJobID, prepared) {
			targets = append(targets, prepared)
			continue
		}
		if !matchesPrebuiltMarketIndexTask(task, dataset, document, imported.ID, installJobID) {
			continue
		}
		var receipt readonlyorm.LazyLLMDocServiceTaskRow
		if err := lazyDB.Order("updated_at DESC, id DESC").Take(&receipt, "kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_REPARSE'",
			dataset.KbID, document.LazyllmDocID, task.LazyllmTaskID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(receipt.Status), "FAILED") {
			targets = append(targets, MarketIndexRetryTarget{ImportTaskID: imported.ID, FailedTaskID: failedTaskID})
		}
	}
	return targets, nil
}

func matchesPrebuiltMarketIndexTask(task orm.Task, dataset orm.Dataset, document orm.Document, importTaskID, installJobID string) bool {
	return task.LazyllmTaskID != "" && matchesPrebuiltMarketIndexOrigin(task, dataset, document, importTaskID, installJobID)
}

func matchesPrebuiltMarketIndexOrigin(task orm.Task, dataset orm.Dataset, document orm.Document, importTaskID, installJobID string) bool {
	var extension taskExt
	return task.TaskType == string(TaskTypeReparse) && task.KbID == dataset.KbID &&
		json.Unmarshal(task.Ext, &extension) == nil && extension.ProcessingLevel == ProcessingLevelIndexed &&
		extension.ProcessingRevision == dataset.ProcessingRevision && extension.MarketIndexOrigin != nil &&
		*extension.MarketIndexOrigin == (marketIndexTaskOrigin{InstallJobID: installJobID, ImportTaskID: importTaskID, DocumentVersion: document.ResourceVersion})
}

func matchesMarketIndexRetryTask(task orm.Task, dataset orm.Dataset, document orm.Document, importTaskID, installJobID string, target MarketIndexRetryTarget) bool {
	if task.ID != target.FailedTaskID {
		return false
	}
	if !target.Unsubmitted {
		return matchesPrebuiltMarketIndexTask(task, dataset, document, importTaskID, installJobID)
	}
	if task.ID == importTaskID {
		return true
	}
	var extension taskExt
	return task.LazyllmTaskID == "" && matchesPrebuiltMarketIndexOrigin(task, dataset, document, importTaskID, installJobID) &&
		json.Unmarshal(task.Ext, &extension) == nil && extension.MarketIndexBatch != nil && !extension.MarketIndexBatch.Started
}

func latestPrebuiltMarketDocumentTask(db *gorm.DB, datasetID, documentID string) (orm.Task, error) {
	var task orm.Task
	err := db.Order("created_at DESC, id DESC").Take(&task, "dataset_id = ? AND doc_id = ? AND deleted_at IS NULL", datasetID, documentID).Error
	return task, err
}
