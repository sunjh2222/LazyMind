package doc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

var ErrPrebuiltUpdateWorkerFailed = errors.New("prebuilt update Worker did not import the candidate")

var prebuiltMarketCandidatePattern = regexp.MustCompile(`^prebuiltv_[A-Za-z0-9_-]{1,118}$`)

// PrebuiltMarketResourceIDs is the original install's deterministic mapping
// from the package identity to the Core document and import task.
func PrebuiltMarketResourceIDs(datasetID, identity string) (documentID, taskID string) {
	digest := sha256.Sum256([]byte(datasetID + ":" + identity))
	return fmt.Sprintf("doc_%x", digest[:16]), fmt.Sprintf("task_%x", digest[16:])
}

type StagedPrebuiltMarketUpdate struct {
	CandidateID   string
	LazyllmTaskID string

	datasetID, kbID, documentID, previousID, taskID string
	tenantID, ownerID                               string
	resourceVersion                                 int64
	ext                                             documentExt
}

// StagePrebuiltMarketUpdate submits a verified package object to the Worker
// without changing the installed document, task or index projection.
func StagePrebuiltMarketUpdate(ctx context.Context, db *gorm.DB, ds *orm.Dataset, current orm.Document,
	candidateID, jobID string, file MarketImportFile) (*StagedPrebuiltMarketUpdate, error) {
	if db == nil || resourceTransaction(db) || ds == nil || ds.ID == "" || ds.KbID == "" || current.DatasetID != ds.ID ||
		current.ID == "" || current.LazyllmDocID == "" || current.DeletedAt != nil ||
		!prebuiltMarketCandidatePattern.MatchString(candidateID) || candidateID == current.ID ||
		candidateID == current.LazyllmDocID || jobID == "" || file.LocalPath == "" || file.PrebuiltPath == "" {
		return nil, fmt.Errorf("invalid prebuilt market update candidate")
	}
	var previous documentExt
	if err := json.Unmarshal(current.Ext, &previous); err != nil || previous.PrebuiltPath == "" {
		return nil, fmt.Errorf("current document is not a prebuilt market import")
	}
	return stagePrebuiltMarketCandidate(ctx, db, ds, current, candidateID, jobID, file, previous.Tags)
}

// StagePrebuiltMarketAddition prepares the new document without publishing a Core row.
func StagePrebuiltMarketAddition(ctx context.Context, db *gorm.DB, ds *orm.Dataset, jobID string,
	file MarketImportFile) (*StagedPrebuiltMarketUpdate, error) {
	if db == nil || resourceTransaction(db) || ds == nil || ds.ID == "" || ds.KbID == "" || ds.CreateUserID == "" ||
		ds.DeletedAt != nil || !isPrebuiltMarketDataset(*ds) || jobID == "" || file.LocalPath == "" || file.PrebuiltPath == "" ||
		strings.TrimSpace(file.PrebuiltIdentity) == "" || strings.TrimSpace(file.DisplayName) == "" {
		return nil, fmt.Errorf("invalid prebuilt market addition candidate")
	}
	documentID, _ := PrebuiltMarketResourceIDs(ds.ID, file.PrebuiltIdentity)
	document := orm.Document{ID: documentID, DisplayName: strings.TrimSpace(file.DisplayName)}
	// An addition has its own identity namespace, independent of the retry job.
	digest := sha256.Sum256([]byte("addition\x00" + documentID))
	return stagePrebuiltMarketCandidate(ctx, db, ds, document, fmt.Sprintf("prebuiltv_%x", digest[:16]), jobID,
		file, normalizeBatchDocumentTags(file.Tags))
}

func stagePrebuiltMarketCandidate(ctx context.Context, db *gorm.DB, ds *orm.Dataset, document orm.Document,
	candidateID, jobID string, file MarketImportFile, tags []string) (*StagedPrebuiltMarketUpdate, error) {
	// Take this admission fence before the external submission. A process
	// restart can otherwise backfill a staged algorithm document as legacy data.
	if err := rejectExternalPublications(db.WithContext(ctx), ds.ID, []string{candidateID}); err != nil {
		return nil, fmt.Errorf("reserve prebuilt update candidate: %w", err)
	}
	displayName := firstNonEmpty(strings.TrimSpace(file.DisplayName), document.DisplayName)
	ext, err := stagePrebuiltMarketFiles(ds, candidateID, file, displayName, tags)
	if err != nil {
		return nil, fmt.Errorf("stage prebuilt update files: %w", err)
	}
	_, taskID := PrebuiltMarketResourceIDs(ds.ID, candidateID)
	candidate := document
	candidate.LazyllmDocID = candidateID
	item := buildAddFileItem(ds.ID, orm.Task{ID: taskID}, candidate, ext, parsePathForAdd(ext))
	externalTaskID, err := submitPrebuiltMarketCandidate(ctx, ds, item, jobID)
	if err != nil {
		return nil, err
	}
	if err := waitPrebuiltMarketWorker(ctx, ds.KbID, candidateID, externalTaskID); err != nil {
		return nil, err
	}
	return &StagedPrebuiltMarketUpdate{
		CandidateID: candidateID, LazyllmTaskID: externalTaskID, taskID: taskID,
		datasetID: ds.ID, kbID: ds.KbID, tenantID: ds.TenantID, ownerID: ds.CreateUserID,
		documentID: document.ID, previousID: document.LazyllmDocID, resourceVersion: document.ResourceVersion, ext: ext,
	}, nil
}

func submitPrebuiltMarketCandidate(ctx context.Context, ds *orm.Dataset, item addFileItem, jobID string) (string, error) {
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		return "", fmt.Errorf("prebuilt update Worker status store is unavailable")
	}
	var previous readonlyorm.LazyLLMDocServiceTaskRow
	err := lazyDB.WithContext(ctx).Where("kb_id = ? AND doc_id = ? AND task_type = ?", ds.KbID, item.DocID, "DOC_ADD").
		Order("created_at DESC, id DESC").Take(&previous).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("find previous prebuilt update submission: %w", err)
	}
	if err == nil {
		switch NormalizeTaskStateForUI(previous.Status) {
		case "FAILED", "CANCELED":
			// A confirmed failure can be resubmitted against the same files.
		default:
			// A previous response may have been lost. Keep observing that task.
			return previous.TaskID, nil
		}
	}
	_, submissionID := PrebuiltMarketResourceIDs(ds.ID, item.DocID+":"+jobID)
	results, err := callExternalAddDocsContext(ctx, addRequest{
		Items: []addFileItem{item}, KbID: ds.KbID, SourceType: "EXTERNAL",
		IdempotencyKey: parseSubmissionKey(ds.ID, []orm.Task{{ID: submissionID}}), ProcessingLevel: ProcessingLevelChunked,
	})
	if err != nil {
		return "", fmt.Errorf("submit prebuilt update candidate: %w", err)
	}
	if len(results) != 1 || results[0].DocID != item.DocID || strings.TrimSpace(results[0].TaskID) == "" {
		return "", fmt.Errorf("DocServer did not confirm the prebuilt update candidate")
	}
	return strings.TrimSpace(results[0].TaskID), nil
}

func stagePrebuiltMarketFiles(ds *orm.Dataset, candidateID string, file MarketImportFile, displayName string,
	tags []string) (documentExt, error) {
	finalDir, err := filepath.Abs(buildDatasetDocFileDir(ds.TenantID, ds.ID, file.RelativePath, candidateID))
	if err != nil {
		return documentExt{}, err
	}
	parent := filepath.Dir(finalDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return documentExt{}, err
	}
	temp, err := os.MkdirTemp(parent, "prebuilt-update-")
	if err != nil {
		return documentExt{}, err
	}
	defer os.RemoveAll(temp)
	const sidecar = "prebuilt.jsonl.gz"
	storedName := storedFileName(displayName, candidateID)
	if filepath.Ext(storedName) != ".md" || filepath.Base(storedName) != storedName {
		return documentExt{}, fmt.Errorf("prebuilt update requires a Markdown preview")
	}
	size, err := copyMarketFile(file.LocalPath, filepath.Join(temp, storedName))
	if err != nil {
		return documentExt{}, err
	}
	if _, err := copyMarketFile(file.PrebuiltPath, filepath.Join(temp, sidecar)); err != nil {
		return documentExt{}, err
	}
	// The algorithm worker uses the shared upload directory, just as it does
	// for a first install. Keep the normal document-directory access mode.
	if err := os.Chmod(temp, 0o755); err != nil {
		return documentExt{}, err
	}
	if err := os.Rename(temp, finalDir); err != nil {
		if info, statErr := os.Lstat(finalDir); statErr != nil || !info.IsDir() {
			return documentExt{}, err
		}
		// The same logical attempt can be replayed while its Worker still has
		// the existing files open. Verify those bytes and leave them untouched.
		for _, name := range []string{storedName, sidecar} {
			prepared, readErr := prebuiltMarketFileDigest(filepath.Join(temp, name))
			if readErr != nil {
				return documentExt{}, readErr
			}
			existing, readErr := prebuiltMarketFileDigest(filepath.Join(finalDir, name))
			if readErr != nil || existing != prepared {
				return documentExt{}, fmt.Errorf("existing prebuilt update candidate differs from its verified package")
			}
		}
	}
	ext := newDocumentExt(filepath.Join(finalDir, storedName), storedName, displayName, size,
		"text/markdown; charset=utf-8", file.RelativePath, tags)
	ext.PrebuiltPath = filepath.Join(finalDir, sidecar)
	ext.PrebuiltSourcePath, ext.PrebuiltSourceURL = file.SourcePath, file.SourceURL
	ext.PrebuiltVersioned = true
	return ext, nil
}

func prebuiltMarketFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid prebuilt update candidate file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// PrebuiltMarketObjectMatches checks whether a retained local artifact already matches a verified release object.
func PrebuiltMarketObjectMatches(document orm.Document, expected string) bool {
	var ext documentExt
	if expected == "" || json.Unmarshal(document.Ext, &ext) != nil || ext.PrebuiltPath == "" {
		return false
	}
	digest, err := prebuiltMarketFileDigest(ext.PrebuiltPath)
	return err == nil && digest == expected
}

func waitPrebuiltMarketWorker(ctx context.Context, kbID, docID, taskID string) error {
	lazyDB := store.LazyLLMDB()
	if lazyDB == nil {
		return fmt.Errorf("prebuilt update Worker status store is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	for {
		var task readonlyorm.LazyLLMDocServiceTaskRow
		err := lazyDB.WithContext(ctx).Where("kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = ?",
			kbID, docID, taskID, "DOC_ADD").Take(&task).Error
		if err == nil {
			switch NormalizeTaskStateForUI(task.Status) {
			case "SUCCESS":
				if (task.ErrorCode != nil && strings.TrimSpace(*task.ErrorCode) != "" && *task.ErrorCode != "200") ||
					(task.ErrorMsg != nil && strings.TrimSpace(*task.ErrorMsg) != "" &&
						!strings.EqualFold(strings.TrimSpace(*task.ErrorMsg), "success")) {
					return ErrPrebuiltUpdateWorkerFailed
				}
				return nil
			case "FAILED", "CANCELED":
				return ErrPrebuiltUpdateWorkerFailed
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("observe prebuilt update Worker: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for prebuilt update Worker: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// CommitPrebuiltMarketUpdate promotes a successful Worker result in the
// caller's install transaction. It does not delete either remote version.
func CommitPrebuiltMarketUpdate(ctx context.Context, tx *gorm.DB, ds *orm.Dataset, current orm.Document,
	oldTaskID string, staged *StagedPrebuiltMarketUpdate) (string, error) {
	if tx == nil || !resourceTransaction(tx) || ds == nil || staged == nil || oldTaskID == "" ||
		staged.datasetID != ds.ID || staged.kbID != ds.KbID ||
		staged.documentID != current.ID || staged.previousID != current.LazyllmDocID ||
		staged.resourceVersion != current.ResourceVersion || staged.LazyllmTaskID == "" {
		return "", fmt.Errorf("prebuilt update no longer matches the installed document")
	}
	tx = tx.WithContext(ctx)
	var oldTask orm.Task
	if err := tx.Where("id = ? AND dataset_id = ? AND doc_id = ? AND deleted_at IS NULL",
		oldTaskID, ds.ID, current.ID).Take(&oldTask).Error; err != nil {
		return "", fmt.Errorf("find previous prebuilt import task: %w", err)
	}
	if oldTask.TaskType != string(TaskTypeParseUploaded) {
		return "", fmt.Errorf("previous market task was not the document import")
	}
	if err := validateTaskPublication(tx, ds.ID, oldTask, current); err != nil {
		return "", err
	}
	// A superseded algorithm document must not become a new historical owner
	// if a later startup runs backfill over an uninitialized dataset.
	if err := rejectExternalPublications(tx, ds.ID, []string{current.LazyllmDocID}); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	updated := tx.Model(&orm.Document{}).Where("id = ? AND dataset_id = ? AND deleted_at IS NULL AND resource_version = ? AND lazyllm_doc_id = ?",
		current.ID, ds.ID, current.ResourceVersion, current.LazyllmDocID).Updates(map[string]any{
		"lazyllm_doc_id": staged.CandidateID, "resource_version": gorm.Expr("resource_version + 1"),
		"ext": mustJSON(staged.ext), "updated_at": now,
	})
	if updated.Error != nil {
		return "", updated.Error
	}
	if updated.RowsAffected != 1 {
		return "", fmt.Errorf("prebuilt document changed before update publication")
	}
	metadata := taskExt{
		ProcessingLevel: ProcessingLevelChunked, ProcessingRevision: ds.ProcessingRevision,
		TaskType: oldTask.TaskType, TaskState: string(TaskStateSucceeded), MarketSubmission: "submitted",
		DisplayName: current.DisplayName, DocumentPID: current.PID, DataSourceType: "MARKET",
		Files: []TaskFile{{DisplayName: current.DisplayName, StoredName: staged.ext.StoredName, StoredPath: staged.ext.StoredPath,
			FileSize: staged.ext.FileSize, RelativePath: staged.ext.RelativePath, ContentType: staged.ext.ContentType}},
	}
	newTask := orm.Task{
		ID: staged.taskID, DocID: current.ID, LazyllmTaskID: staged.LazyllmTaskID, KbID: ds.KbID,
		AlgoID: oldTask.AlgoID, DatasetID: ds.ID, TaskType: oldTask.TaskType, DisplayName: current.DisplayName,
		DocumentPID: current.PID, Ext: mustJSON(metadata),
		BaseModel: orm.BaseModel{CreateUserID: oldTask.CreateUserID, CreateUserName: oldTask.CreateUserName,
			CreatedAt: now, UpdatedAt: now},
	}
	if err := tx.Create(&newTask).Error; err != nil {
		return "", err
	}
	retired := tx.Model(&orm.Task{}).Where("id = ? AND dataset_id = ? AND doc_id = ? AND deleted_at IS NULL",
		oldTaskID, ds.ID, current.ID).Updates(map[string]any{"deleted_at": now, "updated_at": now})
	if retired.Error != nil {
		return "", retired.Error
	}
	if retired.RowsAffected != 1 {
		return "", fmt.Errorf("previous prebuilt import task changed during publication")
	}
	state := tx.Model(&orm.DocumentProcessingState{}).Where("dataset_id = ? AND document_id = ?", ds.ID, current.ID).
		Updates(map[string]any{
			"parse_status": "succeeded", "chunk_status": "succeeded", "index_status": "pending",
			"parse_error_code": "", "parse_error_message": "", "chunk_error_code": "", "chunk_error_message": "",
			"index_error_code": "", "index_error_message": "", "index_fingerprint": "", "index_artifact_ref": "",
			"embedding_version": embeddingVersionReembedManual, "source_fingerprint": "", "parse_fingerprint": "", "chunk_fingerprint": "",
			"parse_artifact_ref": "", "chunk_artifact_ref": "", "parser_version": "", "chunker_version": "",
			"revision": gorm.Expr("revision + 1"), "updated_at": now,
		})
	if state.Error != nil {
		return "", state.Error
	}
	if state.RowsAffected != 1 {
		return "", fmt.Errorf("prebuilt document processing state disappeared before publication")
	}
	return newTask.ID, nil
}

// CommitPrebuiltMarketAddition publishes an imported document in the caller's install transaction.
func CommitPrebuiltMarketAddition(ctx context.Context, tx *gorm.DB, ds *orm.Dataset, userID, userName string,
	staged *StagedPrebuiltMarketUpdate) (string, error) {
	if tx == nil || !resourceTransaction(tx) || ds == nil || ds.DeletedAt != nil || !isPrebuiltMarketDataset(*ds) ||
		userID == "" || ds.CreateUserID != userID || staged == nil || staged.ownerID != userID || staged.tenantID != ds.TenantID ||
		staged.datasetID != ds.ID || staged.kbID != ds.KbID || staged.previousID != "" || staged.documentID == "" ||
		staged.LazyllmTaskID == "" || !prebuiltMarketCandidatePattern.MatchString(staged.CandidateID) {
		return "", fmt.Errorf("prebuilt addition no longer matches the installed knowledge base")
	}
	tx = tx.WithContext(ctx)
	now := time.Now().UTC()
	owner := orm.BaseModel{CreateUserID: userID, CreateUserName: userName, CreatedAt: now, UpdatedAt: now}
	ext := staged.ext
	name := ext.OriginalFilename
	document := orm.Document{
		ID: staged.documentID, LazyllmDocID: staged.CandidateID, ResourceVersion: 1,
		DatasetID: ds.ID, DisplayName: name, DocumentType: fileDocumentTypeFromName(name),
		Tags: mustJSON(ext.Tags), FileID: staged.documentID, PDFConvertResult: ext.ConvertStatus,
		Ext: mustJSON(ext), BaseModel: owner,
	}
	if err := tx.Create(&document).Error; err != nil {
		return "", err
	}
	taskType := string(TaskTypeParseUploaded)
	metadata := taskExt{
		ProcessingLevel: ProcessingLevelChunked, ProcessingRevision: ds.ProcessingRevision,
		TaskType: taskType, TaskState: string(TaskStateSucceeded), MarketSubmission: "submitted",
		DisplayName: name, DataSourceType: "MARKET", DocumentTags: ext.Tags,
		Files: []TaskFile{{DisplayName: name, StoredName: ext.StoredName, StoredPath: ext.StoredPath,
			FileSize: ext.FileSize, RelativePath: ext.RelativePath, ContentType: ext.ContentType}},
	}
	task := orm.Task{
		ID: staged.taskID, DocID: document.ID, LazyllmTaskID: staged.LazyllmTaskID, KbID: ds.KbID,
		AlgoID: parseDatasetAlgo(ds.Ext).AlgoID, DatasetID: ds.ID, TaskType: taskType,
		DisplayName: name, Ext: mustJSON(metadata), BaseModel: owner,
	}
	if err := tx.Create(&task).Error; err != nil {
		return "", err
	}
	state := newDocumentProcessingState(ds.ID, document.ID, now)
	state.ParseStatus, state.ChunkStatus = "succeeded", "succeeded"
	state.EmbeddingVersion = embeddingVersionReembedManual
	if err := tx.Create(&state).Error; err != nil {
		return "", err
	}
	return task.ID, nil
}
