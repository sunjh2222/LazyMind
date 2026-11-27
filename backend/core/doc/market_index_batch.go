package doc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

// Store the idempotency key and response order, without persisting credentials
// or treating a transport error as proof that DocServer rejected the batch.
type marketIndexBatch struct {
	Key      string `json:"key"`
	Position int    `json:"position"`
	Size     int    `json:"size"`
	Started  bool   `json:"started,omitempty"`
}

func saveMarketIndexBatch(tx *gorm.DB, datasetID string, tasks []orm.Task) error {
	key := parseSubmissionKey(datasetID, tasks)
	for i := range tasks {
		var ext taskExt
		if err := json.Unmarshal(tasks[i].Ext, &ext); err != nil {
			return err
		}
		ext.MarketIndexBatch = &marketIndexBatch{Key: key, Position: i, Size: len(tasks)}
		tasks[i].Ext = mustJSON(ext)
		if err := tx.Model(&orm.Task{}).Where("id = ?", tasks[i].ID).UpdateColumn("ext", tasks[i].Ext).Error; err != nil {
			return err
		}
	}
	return nil
}

func startMarketIndexBatch(ctx context.Context, db *gorm.DB, datasetID string, tasks []orm.Task, authorize func(*gorm.DB) error) error {
	updates := make([]json.RawMessage, len(tasks))
	err := common.ImmediateTransactionWithSQLiteBusyRetry(ctx, db, func(tx *gorm.DB) error {
		// Acquire the market/job/model locks before touching the registered tasks.
		if err := authorize(tx); err != nil {
			return err
		}
		for i := range tasks {
			var current orm.Task
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current,
				"id = ? AND dataset_id = ? AND deleted_at IS NULL", tasks[i].ID, datasetID).Error; err != nil {
				return err
			}
			var ext taskExt
			if json.Unmarshal(current.Ext, &ext) != nil || ext.MarketIndexBatch == nil || ext.MarketIndexBatch.Started || current.LazyllmTaskID != "" {
				return errors.New("prebuilt market reembed batch is no longer available to send")
			}
			var bound int64
			if err := tx.Model(&orm.DocumentProcessingState{}).Where("dataset_id = ? AND document_id = ? AND index_artifact_ref = ?",
				datasetID, current.DocID, "core-task:"+current.ID).Count(&bound).Error; err != nil {
				return err
			}
			if bound != 1 {
				return errMarketIndexRetryTargetChanged
			}
			ext.MarketIndexBatch.Started = true
			updates[i] = mustJSON(ext)
			if err := tx.Model(&current).UpdateColumn("ext", updates[i]).Error; err != nil {
				return err
			}
		}
		// A lease that expires during registration rolls back the send marker.
		return authorize(tx)
	})
	if err == nil {
		for i := range tasks {
			tasks[i].Ext = updates[i]
		}
	}
	return err
}

type marketIndexReceiptRecord struct {
	ResponseJSON *string
}

// RecoverPrebuiltMarketIndexReceipts attaches authoritative, previously accepted
// receipts. Missing or unfinished idempotency records are never replayed.
func RecoverPrebuiltMarketIndexReceipts(ctx context.Context, db *gorm.DB, datasetID, installJobID string, imports []orm.Task) (int, error) {
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		return 0, errors.New("prebuilt market index receipt store is unavailable")
	}
	db, lazyDB = db.WithContext(ctx), lazyDB.WithContext(ctx)
	var dataset orm.Dataset
	if err := db.Take(&dataset, "id = ? AND deleted_at IS NULL", datasetID).Error; err != nil {
		return 0, err
	}
	table := readonlyorm.Table(readonlyorm.LazyLLMSchema(), "lazyllm_idempotency_records")
	var tableChecked bool
	responses := make(map[string][]string)
	recovered := 0
	for _, imported := range imports {
		var document orm.Document
		if err := db.Take(&document, "id = ? AND dataset_id = ? AND deleted_at IS NULL", imported.DocID, datasetID).Error; err != nil {
			return recovered, err
		}
		task, err := latestPrebuiltMarketDocumentTask(db, datasetID, document.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return recovered, err
		}
		var ext taskExt
		if task.LazyllmTaskID != "" || !matchesPrebuiltMarketIndexOrigin(task, dataset, document, imported.ID, installJobID) ||
			json.Unmarshal(task.Ext, &ext) != nil || ext.MarketIndexBatch == nil || !ext.MarketIndexBatch.Started {
			continue
		}
		batch := ext.MarketIndexBatch
		if batch.Key == "" || batch.Size < 1 || batch.Size > 20 || batch.Position < 0 || batch.Position >= batch.Size {
			continue
		}
		if !tableChecked {
			tableChecked = true
			if !lazyDB.Migrator().HasTable(table) {
				return recovered, nil
			}
		}
		ids, checked := responses[batch.Key]
		if !checked {
			var record marketIndexReceiptRecord
			err = lazyDB.Table(table).Take(&record, "endpoint = ? AND idempotency_key = ? AND status = ?",
				"/v1/docs/reparse", batch.Key, "COMPLETED").Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return recovered, err
			}
			if err == nil && record.ResponseJSON != nil {
				var response reparseResponse
				if json.Unmarshal([]byte(*record.ResponseJSON), &response) == nil {
					ids = parseReparseTaskIDs(response)
				}
			}
			responses[batch.Key] = ids
		}
		if len(ids) != batch.Size {
			continue
		}
		id := strings.TrimSpace(ids[batch.Position])
		if id == "" {
			continue
		}
		var receipt readonlyorm.LazyLLMDocServiceTaskRow
		err = lazyDB.Take(&receipt, "kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_REPARSE'",
			dataset.KbID, document.LazyllmDocID, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return recovered, err
		}
		bound, err := bindRecoveredMarketIndexReceipt(ctx, db, dataset, document, task, *batch, id)
		if err != nil {
			return recovered, err
		}
		if bound {
			recovered++
		}
	}
	return recovered, nil
}

func bindRecoveredMarketIndexReceipt(ctx context.Context, db *gorm.DB, dataset orm.Dataset, document orm.Document, task orm.Task,
	batch marketIndexBatch, id string) (bool, error) {
	bound := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateTaskPublication(tx, dataset.ID, task, document); err != nil {
			return err
		}
		var currentDataset orm.Dataset
		if err := tx.Take(&currentDataset, "id = ? AND deleted_at IS NULL", dataset.ID).Error; err != nil {
			return err
		}
		if currentDataset.KbID != dataset.KbID || currentDataset.ProcessingRevision != dataset.ProcessingRevision {
			return nil
		}
		var current orm.Document
		if err := tx.Take(&current, "id = ? AND dataset_id = ? AND deleted_at IS NULL", document.ID, dataset.ID).Error; err != nil {
			return err
		}
		var state orm.DocumentProcessingState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&state, "dataset_id = ? AND document_id = ?", dataset.ID, current.ID).Error; err != nil {
			return err
		}
		if current.LazyllmDocID != document.LazyllmDocID || state.IndexArtifactRef != "core-task:"+task.ID {
			return nil
		}
		var latest orm.Task
		if err := tx.Take(&latest, "id = ? AND dataset_id = ? AND deleted_at IS NULL", task.ID, dataset.ID).Error; err != nil {
			return err
		}
		var extension taskExt
		if latest.LazyllmTaskID != "" || json.Unmarshal(latest.Ext, &extension) != nil ||
			extension.MarketIndexBatch == nil || *extension.MarketIndexBatch != batch {
			return nil
		}
		extension.TaskState = string(TaskStateRunning)
		extension.ErrorMessage = ""
		write := tx.Model(&latest).Where("COALESCE(lazyllm_task_id, '') = ''").Updates(map[string]any{
			"ext": mustJSON(extension), "lazyllm_task_id": id, "updated_at": time.Now().UTC(),
		})
		bound = write.RowsAffected == 1
		return write.Error
	})
	return bound && err == nil, err
}
