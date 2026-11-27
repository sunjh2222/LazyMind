package doc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"lazymind/core/common/orm"
	"lazymind/core/store"
)

const (
	marketImportBatchSize  = 50
	marketImportBatchDelay = 200 * time.Millisecond
)

// MarketImportFile is one downloaded file to import into a dataset.
type MarketImportFile struct {
	LocalPath        string   // absolute path of the downloaded file
	DisplayName      string   // user-facing file name
	RelativePath     string   // path inside the package ("" when at the root)
	Tags             []string // document tags assigned by the source adapter
	PrebuiltPath     string   // validated gzip JSONL staged by the market installer
	PrebuiltIdentity string
	SourcePath       string
	SourceURL        string
}

// MarketImportResult summarizes a submitted market import.
type MarketImportResult struct {
	DatasetID         string              `json:"dataset_id"`
	DispatchedTaskIDs []string            `json:"dispatched_task_ids,omitempty"`
	Submitted         int                 `json:"submitted"`
	TaskIDs           []string            `json:"task_ids"`
	Failures          []MarketFileFailure `json:"failures,omitempty"`
}

type prebuiltRetryAttemptKey struct{}

// MarketFileFailure contains user-safe metadata, never raw provider errors.
type MarketFileFailure struct {
	TaskID string `json:"task_id,omitempty"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ImportMarketFiles registers document/task rows for every downloaded file and
// submits them to the parsing pipeline (parse + vectorize). It mirrors the
// multipart upload flow but sources the bytes from local files.
func ImportMarketFiles(ctx context.Context, ds *orm.Dataset, userID, userName string, files []MarketImportFile) (*MarketImportResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ds == nil || len(files) == 0 {
		return nil, fmt.Errorf("dataset and files are required")
	}
	db := store.DB()
	if db == nil {
		return nil, fmt.Errorf("store not initialized")
	}

	// startTasksInternal needs a request for user context and the parsing
	// service call; a synthetic request carries the same ctx and user headers.
	r := (&http.Request{Header: make(http.Header)}).WithContext(ctx)
	r.Header.Set("X-User-Id", userID)
	r.Header.Set("X-User-Name", userName)

	result := &MarketImportResult{DatasetID: ds.ID, TaskIDs: make([]string, 0, len(files))}
	var toSubmit []string
	defer func() { _ = MarketImportCheckpoint(ctx, result) }()
	for start := 0; start < len(files); start += marketImportBatchSize {
		end := start + marketImportBatchSize
		if end > len(files) {
			end = len(files)
		}
		for _, file := range files[start:end] {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			id, pending, err := registerMarketFile(ctx, db, ds, userID, userName, file)
			if err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				name := file.DisplayName
				if name == "" {
					name = filepath.Base(file.LocalPath)
				}
				result.Failures = append(result.Failures, MarketFileFailure{Name: name, Reason: "import_failed"})
				continue
			}
			result.TaskIDs = append(result.TaskIDs, id)
			if pending {
				toSubmit = append(toSubmit, id)
			}
		}
		if err := MarketImportCheckpoint(ctx, result); err != nil {
			return result, err
		}
	}
	// Registration failures are isolated to their files. Submission failures
	// are persisted on their task rows, so successful files remain usable.
	for start := 0; start < len(toSubmit); start += marketImportBatchSize {
		end := start + marketImportBatchSize
		if end > len(toSubmit) {
			end = len(toSubmit)
		}
		if err := MarketImportCheckpoint(ctx, result); err != nil {
			return result, err
		}
		result.DispatchedTaskIDs = append(result.DispatchedTaskIDs, toSubmit[start:end]...)
		if err := MarketImportCheckpoint(ctx, result); err != nil {
			return result, err
		}
		submitted, err := submitMarketTasks(r, ds.ID, toSubmit[start:end])
		result.Submitted += submitted
		if err != nil {
			return result, err
		}
		if end < len(toSubmit) {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(marketImportBatchDelay):
			}
		}
	}
	return result, nil
}

// registerMarketFile owns one file's filesystem and database registration.
func registerMarketFile(ctx context.Context, db *gorm.DB, ds *orm.Dataset, userID, userName string, file MarketImportFile) (string, bool, error) {
	now := time.Now().UTC()
	displayName := strings.TrimSpace(file.DisplayName)
	if displayName == "" {
		displayName = filepath.Base(file.LocalPath)
	}
	documentTags := normalizeBatchDocumentTags(file.Tags)
	documentID := newDocID()
	taskID := newTaskID()
	if file.PrebuiltPath != "" {
		documentID, taskID = PrebuiltMarketResourceIDs(ds.ID, file.PrebuiltIdentity)
		var existing orm.Task
		err := db.WithContext(ctx).Where("id = ? AND dataset_id = ? AND doc_id = ? AND deleted_at IS NULL", taskID, ds.ID, documentID).Take(&existing).Error
		if err == nil {
			return existing.ID, strings.TrimSpace(existing.LazyllmTaskID) == "", nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", false, err
		}
	}
	storedName := storedFileName(displayName, documentID)
	finalDir := buildDatasetDocFileDir(ds.TenantID, ds.ID, file.RelativePath, documentID)
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		return "", false, fmt.Errorf("create dataset dir failed: %w", err)
	}
	registered := false
	defer func() {
		if !registered {
			_ = os.RemoveAll(finalDir)
		}
	}()
	finalPath := filepath.Join(finalDir, storedName)
	size, err := copyMarketFile(file.LocalPath, finalPath)
	if err != nil {
		return "", false, fmt.Errorf("copy %s failed: %w", displayName, err)
	}
	size, err = normalizeUploadedTextFileInPlace(finalPath, displayName, size)
	if err != nil {
		return "", false, fmt.Errorf("normalize %s failed: %w", displayName, err)
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(displayName)))
	if file.PrebuiltPath != "" {
		contentType = "text/markdown; charset=utf-8"
	} else if contentType == "" {
		contentType = "application/octet-stream"
	}
	docExt := newDocumentExt(finalPath, storedName, displayName, size, contentType, file.RelativePath, nil)
	if file.PrebuiltPath != "" {
		docExt.PrebuiltPath, err = filepath.Abs(filepath.Join(finalDir, "prebuilt.jsonl.gz"))
		if err != nil {
			return "", false, err
		}
		if _, err := copyMarketFile(file.PrebuiltPath, docExt.PrebuiltPath); err != nil {
			return "", false, fmt.Errorf("store prebuilt chunks: %w", err)
		}
		docExt.PrebuiltSourcePath = file.SourcePath
		docExt.PrebuiltSourceURL = file.SourceURL
	}
	docRow := orm.Document{
		ID: documentID, DatasetID: ds.ID, DisplayName: displayName,
		DocumentType: fileDocumentTypeFromName(displayName),
		Tags:         mustJSON(documentTags), FileID: documentID,
		PDFConvertResult: docExt.ConvertStatus, Ext: mustJSON(docExt),
		BaseModel: orm.BaseModel{CreateUserID: userID, CreateUserName: userName, CreatedAt: now, UpdatedAt: now},
	}
	tExt := taskExt{
		TaskType: string(TaskTypeParseUploaded), DisplayName: displayName,
		TaskState: "WAITING", MarketSubmission: "registered",
		DataSourceType: "MARKET", DocumentTags: documentTags,
		Files: []TaskFile{{DisplayName: displayName, StoredName: storedName, StoredPath: finalPath, FileSize: size, RelativePath: file.RelativePath, ContentType: contentType}},
	}
	taskRow := orm.Task{
		ID: taskID, DocID: documentID, KbID: ds.ID, AlgoID: datasetAlgoIDByID(ds.ID),
		DatasetID: ds.ID, TaskType: string(TaskTypeParseUploaded),
		DisplayName: displayName, Ext: mustJSON(tExt),
		BaseModel: orm.BaseModel{CreateUserID: userID, CreateUserName: userName, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&docRow).Error; err != nil {
			return err
		}
		if err := tx.Create(&taskRow).Error; err != nil {
			return err
		}
		if file.PrebuiltPath != "" {
			return createDocumentProcessingState(tx, ds.ID, documentID, now)
		}
		return nil
	}); err != nil {
		return "", false, fmt.Errorf("create document/task rows failed: %w", err)
	}
	registered = true
	recalcAffectedFolderStats(ctx, ds.ID, "")
	return taskID, true, nil
}

// RetryMarketTasks resubmits only the specified failed files in their existing
// dataset; successful documents are neither deleted nor imported twice.
func RetryMarketTasks(ctx context.Context, datasetID, userID, userName string, taskIDs []string) (int, error) {
	files, err := MarketTaskStates(ctx, store.DB(), datasetID, taskIDs)
	if err != nil {
		return 0, err
	}
	if len(files) != len(taskIDs) {
		return 0, ErrMarketProcessing
	}
	for _, file := range files {
		if file.State != "FAILED" {
			return 0, ErrMarketProcessing
		}
	}
	// Record the old binding before a new request. Until a new response binds
	// an ID, old FAILED rows cannot establish the outcome of this attempt.
	for _, id := range taskIDs {
		var row orm.Task
		if err := store.DB().WithContext(ctx).Where("id = ? AND dataset_id = ?", id, datasetID).Take(&row).Error; err != nil {
			return 0, err
		}
		var ext taskExt
		if err := json.Unmarshal(row.Ext, &ext); err != nil {
			return 0, err
		}
		ext.MarketPreviousTaskID = row.LazyllmTaskID
		ext.MarketSubmission = "retrying"
		if err := store.DB().WithContext(ctx).Model(&row).Update("ext", mustJSON(ext)).Error; err != nil {
			return 0, err
		}
	}
	ctx = context.WithValue(ctx, marketRetryDispatchKey{}, true)
	r := (&http.Request{Header: make(http.Header)}).WithContext(ctx)
	r.Header.Set("X-User-Id", userID)
	r.Header.Set("X-User-Name", userName)
	return submitMarketTasks(r, datasetID, taskIDs)
}

// HasUnconfirmedPrebuiltRetry checks the persisted submission identity, not
// just UNKNOWN telemetry, before the market can replay a terminal retry.
func HasUnconfirmedPrebuiltRetry(ctx context.Context, db *gorm.DB, datasetID, attempt string, taskIDs []string) bool {
	var tasks []orm.Task
	if err := db.WithContext(ctx).Select("id", "lazyllm_task_id", "ext").
		Where("dataset_id = ? AND id IN ? AND deleted_at IS NULL", datasetID, taskIDs).
		Find(&tasks).Error; err != nil || len(tasks) != len(taskIDs) {
		return false
	}
	pending := false
	for _, task := range tasks {
		var ext taskExt
		if json.Unmarshal(task.Ext, &ext) != nil {
			return false
		}
		if unconfirmedMarketAttempt(task, ext) {
			if ext.MarketRetryAttempt != attempt {
				return false
			}
			pending = true
		}
	}
	return pending
}

// RetryPrebuiltMarketTasks keeps the durable Core document/task IDs. A
// confirmed failure gets a new DocServer submission identity. Retrying the
// same logical attempt after a lost response replays exactly that identity.
func RetryPrebuiltMarketTasks(ctx context.Context, datasetID, userID, userName, attempt string, taskIDs []string) (int, error) {
	if attempt == "" {
		return 0, fmt.Errorf("prebuilt retry attempt is missing")
	}
	db := store.DB()
	files, err := MarketTaskStates(ctx, db, datasetID, taskIDs)
	if err != nil {
		return 0, err
	}
	if len(files) != len(taskIDs) {
		return 0, ErrMarketProcessing
	}
	ctx = context.WithValue(ctx, marketRetryDispatchKey{}, true)
	ctx = context.WithValue(ctx, prebuiltRetryAttemptKey{}, attempt)
	r := (&http.Request{Header: make(http.Header)}).WithContext(ctx)
	r.Header.Set("X-User-Id", userID)
	r.Header.Set("X-User-Name", userName)
	submitted := 0
	for _, file := range files {
		row := file.Task
		var ext taskExt
		if err := json.Unmarshal(row.Ext, &ext); err != nil {
			return submitted, err
		}
		if ext.MarketRetryAttempt != attempt {
			if file.State != "FAILED" {
				return submitted, ErrMarketProcessing
			}
			ext.MarketRetryAttempt = attempt
			ext.MarketPreviousTaskID = row.LazyllmTaskID
			ext.MarketSubmission = "retrying"
			if err := db.WithContext(ctx).Model(&row).Update("ext", mustJSON(ext)).Error; err != nil {
				return submitted, err
			}
		} else if !unconfirmedMarketAttempt(row, ext) {
			submitted++
			continue
		}
		// Per-document submission keeps the request body stable when an earlier
		// file in the same retry has already received its acknowledgement.
		if _, err := startTasksInternal(r, datasetID, []string{row.ID}); err != nil {
			return submitted, fmt.Errorf("submit prebuilt document retry: %w", err)
		}
		submitted++
	}
	return submitted, nil
}

func submitMarketTasks(r *http.Request, datasetID string, taskIDs []string) (int, error) {
	results, _ := startTasksInternal(r, datasetID, taskIDs)
	if r.Context().Err() != nil {
		return 0, r.Context().Err()
	}
	accepted := make(map[string]bool, len(results))
	rejected := make(map[string]bool, len(results))
	for _, result := range results {
		accepted[result.TaskID] = result.Status == "STARTED"
		rejected[result.TaskID] = result.SubmitStatus == "REJECTED"
	}
	submitted := 0
	for _, id := range taskIDs {
		if accepted[id] {
			submitted++
			continue
		}
		var task orm.Task
		if err := store.DB().WithContext(r.Context()).Where("id = ? AND dataset_id = ?", id, datasetID).Take(&task).Error; err != nil {
			return submitted, err
		}
		var ext taskExt
		_ = json.Unmarshal(task.Ext, &ext)
		ext.TaskState = "FAILED"
		if rejected[id] {
			ext.MarketSubmission = "rejected"
			ext.MarketPreviousTaskID = ""
		} else if ext.MarketSubmission != "retrying" {
			ext.MarketSubmission = "submitting"
		}
		ext.ErrorMessage = "Document submission failed; retry this file"
		// Old external task ids must not override the new submission failure.
		if err := store.DB().WithContext(r.Context()).Model(&orm.Task{}).Where("id = ? AND dataset_id = ?", id, datasetID).
			Updates(map[string]any{"ext": mustJSON(ext), "lazyllm_task_id": ""}).Error; err != nil {
			return submitted, err
		}
	}
	return submitted, nil
}

// copyMarketFile copies a downloaded file into the dataset doc dir.
func copyMarketFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return n, nil
}
