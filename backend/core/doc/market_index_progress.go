package doc

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"gorm.io/gorm"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

type MarketIndexReadScope struct {
	InstallJobID, DatasetID, UserID string
	ImportTaskIDs                   []string
}

type MarketIndexObservations struct {
	Done, Failed, Pending, Unknown, Unsubmitted, Canceled int
	Recoverable                                           bool
}

type marketIndexDocumentKey struct{ dataset, document string }
type marketIndexReceiptKey struct{ kb, document, task string }

type marketIndexReadAttempt struct {
	key   marketIndexReceiptKey
	batch *marketIndexBatch
}

// ObservePrebuiltMarketIndexes projects exact Worker receipts for the current
// versions of the originally imported documents, without changing their state.
func ObservePrebuiltMarketIndexes(ctx context.Context, db *gorm.DB, scopes []MarketIndexReadScope) (map[string]MarketIndexObservations, error) {
	result := make(map[string]MarketIndexObservations, len(scopes))
	if len(scopes) == 0 {
		return result, nil
	}
	db = db.WithContext(ctx)
	datasetIDs, importIDs := map[string]bool{}, map[string]bool{}
	for _, scope := range scopes {
		datasetIDs[scope.DatasetID] = true
		for _, id := range scope.ImportTaskIDs {
			importIDs[id] = true
		}
	}
	datasets, err := loadMarketIndexRows[orm.Dataset](db, "id IN ? AND deleted_at IS NULL", slices.Collect(maps.Keys(datasetIDs)))
	if err != nil {
		return nil, err
	}
	datasetByID := make(map[string]orm.Dataset, len(datasets))
	for _, dataset := range datasets {
		if dataset.KbID != "" && dataset.ProcessingLevel == ProcessingLevelChunked && isPrebuiltMarketDataset(dataset) {
			datasetByID[dataset.ID] = dataset
		}
	}
	imports, err := loadMarketIndexRows[orm.Task](db, "id IN ? AND deleted_at IS NULL", slices.Collect(maps.Keys(importIDs)))
	if err != nil {
		return nil, err
	}
	importByID := make(map[string]orm.Task, len(imports))
	docIDs := make(map[string]bool, len(imports))
	for _, task := range imports {
		if task.TaskType == string(TaskTypeParseUploaded) && task.DocID != "" && task.LazyllmTaskID != "" {
			importByID[task.ID], docIDs[task.DocID] = task, true
		}
	}
	ids := slices.Collect(maps.Keys(docIDs))
	documents, err := loadMarketIndexRows[orm.Document](db, "id IN ? AND deleted_at IS NULL", ids)
	if err != nil {
		return nil, err
	}
	documentByKey := make(map[marketIndexDocumentKey]orm.Document, len(documents))
	for _, document := range documents {
		var metadata documentExt
		if document.LazyllmDocID != "" && json.Unmarshal(document.Ext, &metadata) == nil && metadata.PrebuiltPath != "" {
			documentByKey[marketIndexDocumentKey{document.DatasetID, document.ID}] = document
		}
	}
	states, err := loadMarketIndexRows[orm.DocumentProcessingState](db, "document_id IN ?", ids)
	if err != nil {
		return nil, err
	}
	stateByKey := make(map[marketIndexDocumentKey]orm.DocumentProcessingState, len(states))
	for _, state := range states {
		stateByKey[marketIndexDocumentKey{state.DatasetID, state.DocumentID}] = state
	}
	tasks, err := loadMarketIndexRows[orm.Task](db.Unscoped().Order("created_at DESC, id DESC"), "doc_id IN ?", ids)
	if err != nil {
		return nil, err
	}
	latestByKey := make(map[marketIndexDocumentKey]orm.Task, len(ids))
	reparseHistory := make(map[marketIndexDocumentKey]bool)
	for _, task := range tasks {
		key := marketIndexDocumentKey{task.DatasetID, task.DocID}
		if task.TaskType == string(TaskTypeReparse) {
			reparseHistory[key] = true
		}
		if task.DeletedAt == nil && latestByKey[key].ID == "" {
			latestByKey[key] = task
		}
	}

	attempts := make(map[string][]marketIndexReadAttempt, len(scopes))
	for _, scope := range scopes {
		var observed MarketIndexObservations
		dataset := datasetByID[scope.DatasetID]
		for _, importID := range scope.ImportTaskIDs {
			imported := importByID[importID]
			key := marketIndexDocumentKey{scope.DatasetID, imported.DocID}
			document, state, latest := documentByKey[key], stateByKey[key], latestByKey[key]
			switch {
			case dataset.ID == "" || dataset.CreateUserID != scope.UserID || imported.DatasetID != dataset.ID ||
				document.ID == "" || state.DocumentID == "" || latest.ID == "":
				observed.Unknown++
			case latest.ID == importID && state.IndexArtifactRef == "" && !reparseHistory[key] &&
				state.IndexStatus != "running" && state.IndexStatus != "succeeded":
				observed.Unsubmitted++
			case state.IndexArtifactRef != "core-task:"+latest.ID ||
				!matchesPrebuiltMarketIndexOrigin(latest, dataset, document, importID, scope.InstallJobID):
				observed.Unknown++
			default:
				var extension taskExt
				_ = json.Unmarshal(latest.Ext, &extension) // Origin verification checked the metadata.
				if latest.LazyllmTaskID == "" && extension.MarketIndexBatch != nil && !extension.MarketIndexBatch.Started {
					observed.Unsubmitted++
					continue
				}
				attempts[scope.InstallJobID] = append(attempts[scope.InstallJobID], marketIndexReadAttempt{
					key: marketIndexReceiptKey{dataset.KbID, document.LazyllmDocID, latest.LazyllmTaskID}, batch: extension.MarketIndexBatch,
				})
			}
		}
		result[scope.InstallJobID] = observed
	}
	return result, observeMarketIndexReceipts(ctx, attempts, result)
}

func observeMarketIndexReceipts(ctx context.Context, attempts map[string][]marketIndexReadAttempt, result map[string]MarketIndexObservations) error {
	if len(attempts) == 0 {
		return nil
	}
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		for id, rows := range attempts {
			observed := result[id]
			observed.Unknown += len(rows)
			result[id] = observed
		}
		return nil
	}
	lazyDB = lazyDB.WithContext(ctx)
	batchKeys := make(map[string]bool)
	for _, rows := range attempts {
		for _, attempt := range rows {
			if attempt.key.task == "" && attempt.batch != nil && attempt.batch.Started && attempt.batch.Key != "" {
				batchKeys[attempt.batch.Key] = true
			}
		}
	}
	acceptedBatches, err := loadMarketIndexAcceptedBatches(lazyDB, slices.Collect(maps.Keys(batchKeys)))
	if err != nil {
		return err
	}
	observationKey := func(attempt marketIndexReadAttempt) marketIndexReceiptKey {
		key, batch := attempt.key, attempt.batch
		if key.task == "" && batch != nil && batch.Started && batch.Size > 0 && batch.Size <= 20 && batch.Position >= 0 && batch.Position < batch.Size {
			if ids := acceptedBatches[batch.Key]; len(ids) == batch.Size {
				key.task = strings.TrimSpace(ids[batch.Position])
			}
		}
		return key
	}
	receiptIDs := make(map[string]bool)
	for _, rows := range attempts {
		for _, attempt := range rows {
			if key := observationKey(attempt); key.task != "" {
				receiptIDs[key.task] = true
			}
		}
	}
	receipts, err := loadMarketIndexRows[readonlyorm.LazyLLMDocServiceTaskRow](lazyDB.Order("updated_at DESC, id DESC"),
		"task_id IN ? AND task_type = 'DOC_REPARSE'", slices.Collect(maps.Keys(receiptIDs)))
	if err != nil {
		return err
	}
	statuses := make(map[marketIndexReceiptKey]string, len(receipts))
	for _, receipt := range receipts {
		key := marketIndexReceiptKey{receipt.KbID, receipt.DocID, receipt.TaskID}
		if _, seen := statuses[key]; !seen {
			statuses[key] = strings.ToUpper(strings.TrimSpace(receipt.Status))
		}
	}
	for id, rows := range attempts {
		observed := result[id]
		for _, attempt := range rows {
			status := statuses[observationKey(attempt)]
			switch {
			case status == "WAITING" || status == "WORKING":
				observed.Pending++
			case attempt.key.task == "":
				observed.Unknown++
				observed.Recoverable = observed.Recoverable || status == "SUCCESS" || status == "FAILED"
			case status == "SUCCESS":
				observed.Done++
			case status == "FAILED":
				observed.Failed++
			case status == "CANCELLED" || status == "CANCELED":
				observed.Canceled++
			default:
				observed.Unknown++
			}
		}
		result[id] = observed
	}
	return nil
}

func loadMarketIndexAcceptedBatches(db *gorm.DB, keys []string) (map[string][]string, error) {
	accepted := make(map[string][]string)
	table := readonlyorm.Table(readonlyorm.LazyLLMSchema(), "lazyllm_idempotency_records")
	if len(keys) == 0 || !db.Migrator().HasTable(table) {
		return accepted, nil
	}
	type responseRow struct {
		IdempotencyKey string
		ResponseJSON   *string
	}
	for batch := range slices.Chunk(keys, 500) {
		var rows []responseRow
		if err := db.Table(table).Where("endpoint = '/v1/docs/reparse' AND status = 'COMPLETED' AND idempotency_key IN ?", batch).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			var response reparseResponse
			if row.ResponseJSON != nil && json.Unmarshal([]byte(*row.ResponseJSON), &response) == nil {
				accepted[row.IdempotencyKey] = parseReparseTaskIDs(response)
			}
		}
	}
	return accepted, nil
}

func loadMarketIndexRows[T any](db *gorm.DB, condition string, ids []string) ([]T, error) {
	var rows []T
	for ids := range slices.Chunk(ids, 500) {
		var batch []T
		if err := db.Where(condition, ids).Find(&batch).Error; err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
	}
	return rows, nil
}
