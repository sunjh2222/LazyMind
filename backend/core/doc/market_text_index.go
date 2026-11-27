package doc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
	"lazymind/core/systemdeps"
)

// MarketEmbeddingIdentity is the model and vector generation confirmed for a market install.
type MarketEmbeddingIdentity struct {
	Source     string
	ModelID    string
	Signature  string
	Generation int64
}

var ErrMarketEmbeddingChanged = errors.New("the confirmed market embedding model is no longer available")
var errMarketIndexRetryTargetChanged = errors.New("prebuilt market index retry target changed before dispatch")

func readMarketEmbeddingIdentity(ctx context.Context, db *gorm.DB, expected MarketEmbeddingIdentity) (orm.EmbeddingIndexState, error) {
	var state orm.EmbeddingIndexState
	if err := db.WithContext(ctx).Take(&state, "scope = ?", "global").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return state, ErrMarketEmbeddingChanged
		}
		return state, err
	}
	if (expected.Source != "own" && expected.Source != "cloud") || expected.ModelID == "" || expected.Signature == "" || expected.Generation <= 0 ||
		state.Phase != "ready" || state.ActiveSource != expected.Source || state.ActiveModelID != expected.ModelID ||
		state.Signature != expected.Signature || state.Generation != expected.Generation {
		return state, ErrMarketEmbeddingChanged
	}
	return state, nil
}

func loadMarketReembedConfig(ctx context.Context, db *gorm.DB, userID string, expected MarketEmbeddingIdentity) (map[string]any, error) {
	before, err := readMarketEmbeddingIdentity(ctx, db, expected)
	if err != nil {
		return nil, err
	}
	config, err := loadParsingLLMConfig(ctx, db, userID, ProcessingLevelIndexed)
	if err != nil {
		return nil, ErrMarketEmbeddingChanged
	}
	after, err := readMarketEmbeddingIdentity(ctx, db, expected)
	if err != nil {
		return nil, err
	}
	role, _ := config["embed_main"].(map[string]any)
	source, _ := role["source"].(string)
	model, _ := role["model"].(string)
	if strings.TrimSpace(source) == "" || strings.TrimSpace(model) == "" || !before.UpdatedAt.Equal(after.UpdatedAt) {
		return nil, ErrMarketEmbeddingChanged
	}
	return map[string]any{"embed_main": role, "_embedding_generation": expected.Generation}, nil
}

// SubmitPrebuiltMarketReembed indexes a prebuilt import, or resumes the explicitly failed retry targets.
// validateSource claims the dispatch once, and validateBatch rechecks that claim for each batch.
func SubmitPrebuiltMarketReembed(ctx context.Context, db *gorm.DB, datasetID, userID, userName string,
	imports []orm.Task, expected MarketEmbeddingIdentity, installJobID string, retryTargets []MarketIndexRetryTarget,
	validateSource func(context.Context) error, validateBatch func(context.Context, *gorm.DB) error) error {
	if db == nil || resourceTransaction(db) || userID == "" || installJobID == "" || len(imports) == 0 || validateSource == nil || validateBatch == nil {
		return errors.New("invalid prebuilt market indexing request")
	}
	if !systemdeps.PythonComponentActive("rag") {
		return errors.New("RAG component is required for prebuilt market indexing")
	}
	var dataset orm.Dataset
	if err := db.WithContext(ctx).Take(&dataset, "id = ? AND create_user_id = ? AND deleted_at IS NULL", datasetID, userID).Error; err != nil {
		return err
	}
	if dataset.KbID == "" || dataset.ProcessingLevel != ProcessingLevelChunked || !isPrebuiltMarketDataset(dataset) {
		return errors.New("prebuilt market library no longer accepts this index request")
	}
	var selected map[string]MarketIndexRetryTarget
	if retryTargets != nil {
		if len(retryTargets) == 0 {
			return nil
		}
		current, err := PrebuiltMarketIndexRetryTargets(ctx, db, datasetID, installJobID, imports)
		if err != nil {
			return err
		}
		requested := make(map[MarketIndexRetryTarget]bool, len(retryTargets))
		for _, target := range retryTargets {
			requested[target] = true
		}
		selected = make(map[string]MarketIndexRetryTarget, len(current))
		for _, target := range current {
			if requested[target] {
				selected[target.ImportTaskID] = target
			}
		}
		if len(selected) == 0 {
			return nil
		}
		imports = slices.DeleteFunc(slices.Clone(imports), func(task orm.Task) bool { return selected[task.ID].ImportTaskID == "" })
	}
	documents, err := prebuiltMarketReembedDocuments(ctx, db, dataset, imports)
	if err != nil {
		return err
	}
	config, err := loadMarketReembedConfig(ctx, db, userID, expected)
	if err != nil {
		return err
	}
	if err := validateSource(ctx); err != nil {
		return err
	}
	const batchSize = 20
	for start := 0; start < len(documents); start += batchSize {
		end := min(start+batchSize, len(documents))
		if err := submitPrebuiltMarketReembedBatch(ctx, db, dataset, imports[start:end], documents[start:end], userName, installJobID,
			selected, expected, config, validateBatch); err != nil {
			return err
		}
	}
	return nil
}

func submitPrebuiltMarketReembedBatch(ctx context.Context, db *gorm.DB, dataset orm.Dataset, imports []orm.Task,
	documents []orm.Document, userName, installJobID string, selected map[string]MarketIndexRetryTarget, expected MarketEmbeddingIdentity,
	config map[string]any, validateBatch func(context.Context, *gorm.DB) error) error {
	if err := validateBatch(ctx, db); err != nil {
		return err
	}
	tasks, err := registerPrebuiltMarketReembed(ctx, db, dataset, imports, documents, userName, installJobID, selected, expected)
	if err != nil {
		return err
	}
	docIDs := make([]string, 0, len(documents))
	for _, document := range documents {
		docIDs = append(docIDs, document.LazyllmDocID)
	}
	requestConfig := make(map[string]any, len(config)+1)
	for key, value := range config {
		requestConfig[key] = value
	}
	requestConfig["_market_index_confirmation"] = marketIndexConfirmationConfig(dataset, expected, documents, tasks)
	request := reparseRequest{DocIDs: docIDs, KbID: dataset.KbID, NgNames: []string{"block", "line"}, Strategy: "reembed",
		IdempotencyKey: parseSubmissionKey(dataset.ID, tasks), ModelConfig: requestConfig, ProcessingLevel: ProcessingLevelIndexed}
	if err := startMarketIndexBatch(ctx, db, dataset.ID, tasks, func(tx *gorm.DB) error {
		locked := tx.Clauses(clause.Locking{Strength: "SHARE"})
		if err := validateBatch(ctx, locked); err != nil {
			return err
		}
		_, err := readMarketEmbeddingIdentity(ctx, locked, expected)
		return err
	}); err != nil {
		return err
	}
	// Upstream errors can include request diagnostics. Do not log or persist them with model credentials.
	var response reparseResponse
	postErr := common.ApiPost(ctx, common.JoinURL(parsingServiceEndpoint(), "/v1/docs/reparse"), request, nil, &response, 15*time.Second)
	ids := parseReparseTaskIDs(response)
	if postErr != nil || len(ids) != len(tasks) || slices.ContainsFunc(ids, func(id string) bool { return strings.TrimSpace(id) == "" }) {
		return errors.New("prebuilt market reembed submission has no confirmed receipt")
	}
	now := time.Now().UTC()
	var publishErr error
	for i, task := range tasks {
		publishErr = errors.Join(publishErr, publishReparseSubmission(ctx, db, dataset, task, documents[i], ProcessingLevelIndexed, strings.TrimSpace(ids[i]), now))
	}
	return publishErr
}

func prebuiltMarketReembedDocuments(ctx context.Context, db *gorm.DB, dataset orm.Dataset, imports []orm.Task) ([]orm.Document, error) {
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		return nil, errors.New("prebuilt market Worker status store is unavailable")
	}
	documents := make([]orm.Document, 0, len(imports))
	documentIDs := make([]string, 0, len(imports))
	seenCore, seenExternal := make(map[string]bool), make(map[string]bool)
	for _, imported := range imports {
		if imported.ID == "" || imported.DocID == "" || imported.LazyllmTaskID == "" || imported.DatasetID != dataset.ID ||
			imported.TaskType != string(TaskTypeParseUploaded) || seenCore[imported.DocID] {
			return nil, errors.New("invalid prebuilt market import task for indexing")
		}
		var document orm.Document
		if err := db.WithContext(ctx).Take(&document, "id = ? AND dataset_id = ? AND deleted_at IS NULL", imported.DocID, dataset.ID).Error; err != nil {
			return nil, err
		}
		var metadata documentExt
		if document.LazyllmDocID == "" || seenExternal[document.LazyllmDocID] || isFolderLikeDocument(document) ||
			json.Unmarshal(document.Ext, &metadata) != nil || metadata.PrebuiltPath == "" {
			return nil, errors.New("prebuilt market document no longer has its imported text binding")
		}
		var receipt readonlyorm.LazyLLMDocServiceTaskRow
		if err := lazyDB.WithContext(ctx).Take(&receipt, "kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_ADD'",
			dataset.KbID, document.LazyllmDocID, imported.LazyllmTaskID).Error; err != nil {
			return nil, err
		}
		if NormalizeTaskStateForUI(receipt.Status) != "SUCCESS" ||
			(receipt.ErrorCode != nil && strings.TrimSpace(*receipt.ErrorCode) != "" && *receipt.ErrorCode != "200") ||
			(receipt.ErrorMsg != nil && strings.TrimSpace(*receipt.ErrorMsg) != "" && !strings.EqualFold(strings.TrimSpace(*receipt.ErrorMsg), "success")) {
			return nil, errors.New("prebuilt market text import is not ready to index")
		}
		seenCore[document.ID], seenExternal[document.LazyllmDocID] = true, true
		documents = append(documents, document)
		documentIDs = append(documentIDs, document.ID)
	}
	var states int64
	if err := db.WithContext(ctx).Model(&orm.DocumentProcessingState{}).
		Where("dataset_id = ? AND document_id IN ?", dataset.ID, documentIDs).Count(&states).Error; err != nil {
		return nil, err
	}
	if states != int64(len(documents)) {
		return nil, errors.New("prebuilt market document processing state is missing")
	}
	return documents, nil
}

func registerPrebuiltMarketReembed(ctx context.Context, db *gorm.DB, dataset orm.Dataset, imports []orm.Task,
	documents []orm.Document, userName, installJobID string, selected map[string]MarketIndexRetryTarget, expected MarketEmbeddingIdentity) ([]orm.Task, error) {
	tasks := make([]orm.Task, 0, len(documents))
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := readMarketEmbeddingIdentity(ctx, tx.Clauses(clause.Locking{Strength: "SHARE"}), expected); err != nil {
			return err
		}
		var current orm.Dataset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current,
			"id = ? AND kb_id = ? AND create_user_id = ? AND tenant_id = ? AND deleted_at IS NULL",
			dataset.ID, dataset.KbID, dataset.CreateUserID, dataset.TenantID).Error; err != nil {
			return err
		}
		if current.ProcessingLevel != ProcessingLevelChunked || current.ProcessingRevision != dataset.ProcessingRevision || !isPrebuiltMarketDataset(current) {
			return errors.New("prebuilt market library changed before indexing")
		}
		for i, document := range documents {
			if err := validateTaskPublication(tx, dataset.ID, imports[i], document); err != nil {
				return err
			}
			var original orm.Task
			if err := tx.Take(&original, "id = ? AND dataset_id = ? AND doc_id = ? AND lazyllm_task_id = ? AND deleted_at IS NULL",
				imports[i].ID, dataset.ID, document.ID, imports[i].LazyllmTaskID).Error; err != nil {
				return err
			}
			target, retrying := selected[original.ID]
			if retrying {
				latest, err := latestPrebuiltMarketDocumentTask(tx, dataset.ID, document.ID)
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if !matchesMarketIndexRetryTask(latest, current, document, original.ID, installJobID, target) {
					return errMarketIndexRetryTargetChanged
				}
				if target.Unsubmitted && latest.ID == original.ID {
					var history int64
					if err := tx.Unscoped().Model(&orm.Task{}).Where("dataset_id = ? AND doc_id = ? AND task_type = ?",
						dataset.ID, document.ID, TaskTypeReparse).Count(&history).Error; err != nil {
						return err
					}
					if history != 0 {
						return errMarketIndexRetryTargetChanged
					}
				}
			}
			task := existingDocumentTask(document, dataset.ID, dataset.KbID, original.AlgoID, dataset.CreateUserID, userName,
				newTaskID(), CreateTaskItem{Task: TaskPayload{ReparseMode: "slice_and_embed", ReparseGroups: []string{"block", "line"}}}, string(TaskTypeReparse))
			var metadata taskExt
			_ = json.Unmarshal(task.Ext, &metadata)
			metadata.ProcessingLevel, metadata.ProcessingRevision = ProcessingLevelIndexed, dataset.ProcessingRevision
			metadata.MarketIndexOrigin = &marketIndexTaskOrigin{InstallJobID: installJobID, ImportTaskID: original.ID, DocumentVersion: document.ResourceVersion}
			task.Ext = mustJSON(metadata)
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			// Gate vector visibility until this task succeeds, while preserving the imported text and its Core identity.
			var extension map[string]json.RawMessage
			_ = json.Unmarshal(document.Ext, &extension) // The preflight required valid prebuilt metadata.
			extension["prebuilt_versioned"] = json.RawMessage(`true`)
			result := tx.Model(&orm.Document{}).Where("id = ? AND dataset_id = ? AND lazyllm_doc_id = ? AND resource_version = ? AND deleted_at IS NULL",
				document.ID, dataset.ID, document.LazyllmDocID, document.ResourceVersion).Update("ext", mustJSON(extension))
			if result.Error != nil || result.RowsAffected != 1 {
				return errors.Join(errors.New("prebuilt market document changed before indexing"), result.Error)
			}
			state := tx.Model(&orm.DocumentProcessingState{}).Where("dataset_id = ? AND document_id = ?", dataset.ID, document.ID)
			if retrying {
				if target.Unsubmitted && target.FailedTaskID == original.ID {
					state = state.Where("COALESCE(index_artifact_ref, '') = '' AND index_status NOT IN ?", []string{"succeeded", "running"})
				} else {
					state = state.Where("index_artifact_ref = ?", "core-task:"+target.FailedTaskID)
				}
			}
			result = state.Updates(map[string]any{"parse_status": "succeeded", "chunk_status": "succeeded", "index_status": "pending",
				"index_error_code": "", "index_error_message": "", "index_fingerprint": "",
				"index_artifact_ref": "core-task:" + task.ID, "embedding_version": embeddingVersionReembedManual, "updated_at": task.CreatedAt})
			if result.Error != nil || result.RowsAffected != 1 {
				if retrying && result.Error == nil {
					return errMarketIndexRetryTargetChanged
				}
				return errors.Join(errors.New("prebuilt market document processing state is missing"), result.Error)
			}
			tasks = append(tasks, task)
		}
		return saveMarketIndexBatch(tx, dataset.ID, tasks)
	})
	return tasks, err
}
