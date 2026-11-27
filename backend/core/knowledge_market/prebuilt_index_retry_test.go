package knowledge_market

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lazymind/core/asyncjob"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/doc"
	"lazymind/core/modelconfig"
	"lazymind/core/modelprovider"
)

func TestPrebuiltMarketIndexRetryResubmitsOnlyConfirmedFailedDocument(t *testing.T) {
	const size = 21
	const docOrigin = "http://127.0.0.1:18118"
	const remoteKB = "docserver-batch-library"
	const modelName, generation = "text-embedding-3-small", 7
	root := t.TempDir()
	t.Setenv("LAZYMIND_UPLOAD_ROOT", root)
	t.Setenv("LAZYMIND_RAG_ENABLED", "true")
	t.Setenv("LAZYMIND_RUNTIME_MODE", "local")
	t.Setenv("LAZYMIND_DOCUMENT_SERVICE_URL", docOrigin)
	t.Setenv("LAZYMIND_ALGO_SERVICE_URL", docOrigin)
	stubMarketWorkerHealth(t)

	// Each fixture is independently content-addressed and contains the existing root, block and line.
	var documents []prebuiltDocument
	var objects [][]byte
	for i := 1; i <= size; i++ {
		id, title := fmt.Sprintf("batch-note-%02d", i), fmt.Sprintf("Batch note %02d", i)
		text := fmt.Sprintf("Synthetic reading notice number %02d.", i)
		metadata := previewJSON(t, map[string]string{"source_path": id + ".md"})
		var raw bytes.Buffer
		encoder := json.NewEncoder(&raw)
		for _, node := range []prebuiltNode{
			{ID: "root", Group: "__lazyllm_root__", Number: 1, Text: text, Metadata: metadata},
			{ID: "block", ParentID: "root", Group: "block", Number: 1, Text: text, Metadata: metadata},
			{ID: "line", ParentID: "block", Group: "line", Number: 1, Text: text, Metadata: metadata},
		} {
			if err := encoder.Encode(node); err != nil {
				t.Fatal(err)
			}
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(raw.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		digest := previewHash(compressed.String())
		documents = append(documents, prebuiltDocument{
			ID: id, Title: title, SourcePath: id + ".md", SourceSHA256: previewHash(text), ChunkFingerprint: previewHash(raw.String()),
			ObjectPath: "objects/" + digest + ".jsonl.gz", ObjectSHA256: digest, Size: int64(compressed.Len()), ChunkCount: 2,
		})
		objects = append(objects, compressed.Bytes())
	}
	pin, manifest := previewRelease(t, "v1", documents)
	pin.URL = previewOrigin + "/latest.json"
	db, router := previewMarket(t, pin)
	if err := db.AutoMigrate(&orm.DocumentProcessingState{}, &orm.EmbeddingIndexState{}, &orm.UserModelProvider{},
		&orm.UserModelProviderGroup{}, &orm.UserModelProviderGroupModel{}, &orm.UserSelectedModel{}, &readonlyorm.LazyLLMDocRow{}); err != nil {
		t.Fatal(err)
	}
	save := func(value any) {
		t.Helper()
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	owner := orm.BaseModel{CreateUserID: previewUserID, CreatedAt: now, UpdatedAt: now}
	signature := "v1:4:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64)
	save(&orm.UserModelProvider{ID: "batch-provider", Name: "OpenAI", BaseModel: owner})
	save(&orm.UserModelProviderGroup{ID: "batch-group", UserModelProviderID: "batch-provider", BaseURL: "https://fixture.invalid/v1",
		APIKey: "fixture-key", IsVerified: true, BaseModel: owner})
	save(&orm.UserModelProviderGroupModel{ID: "batch-embed", UserModelProviderID: "batch-provider", UserModelProviderGroupID: "batch-group",
		ProviderName: "OpenAI", Name: modelName, ModelType: "embed", Enabled: true, BaseModel: owner})
	save(&orm.EmbeddingIndexState{Scope: "global", Phase: "ready", ActiveSource: "own", ActiveModelID: "batch-embed",
		ActiveModelName: modelName, Signature: signature, Generation: generation, UpdatedAt: now})
	installed, err := loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
	if err != nil || installed == nil {
		t.Fatalf("could not load fixture install: %v", err)
	}
	cfg := decodeInstallConfig(installed)
	if len(cfg.TaskIDs) != size {
		t.Fatalf("installed text task count=%d, want %d", len(cfg.TaskIDs), size)
	}
	if err := db.Model(&orm.Dataset{}).Where("id = ?", previewDatasetID).
		UpdateColumns(map[string]any{"kb_id": remoteKB, "ext": json.RawMessage(`{"algo_id":"general_algo","prebuilt_market":true}`)}).Error; err != nil {
		t.Fatal(err)
	}
	var wantedIDs []string
	texts := make(map[string]string, size)
	for i, bundled := range documents {
		var task orm.Task
		if err := db.Take(&task, "id = ?", cfg.TaskIDs[i]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&task).UpdateColumns(map[string]any{"algo_id": "general_algo", "kb_id": remoteKB}).Error; err != nil {
			t.Fatal(err)
		}
		remoteID := "remote-" + bundled.ID
		receipt := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).
			Where("task_id = ? AND doc_id = ? AND kb_id = ? AND task_type = 'DOC_ADD' AND status = 'SUCCESS'", task.LazyllmTaskID, task.DocID, previewDatasetID).
			UpdateColumns(map[string]any{"doc_id": remoteID, "kb_id": remoteKB})
		if receipt.Error != nil || receipt.RowsAffected != 1 {
			t.Fatalf("expected an existing successful import for %s: %v", bundled.ID, receipt.Error)
		}
		text, _, err := decodePrebuiltObject(objects[i], bundled)
		if err != nil || !matchesPrebuiltHash([]byte(text), bundled.SourceSHA256) {
			t.Fatalf("verify the fixture's existing slices for %s: %v", bundled.ID, err)
		}
		folder := filepath.Join(root, bundled.ID)
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		original, artifact := filepath.Join(folder, bundled.SourcePath), filepath.Join(folder, "prebuilt.jsonl.gz")
		if err := os.WriteFile(original, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifact, objects[i], 0o600); err != nil {
			t.Fatal(err)
		}
		save(&orm.Document{ID: task.DocID, LazyllmDocID: remoteID, DatasetID: previewDatasetID, DisplayName: bundled.Title + ".md",
			BaseModel: owner, Ext: previewJSON(t, map[string]any{"stored_path": original, "original_filename": bundled.SourcePath,
				"content_type": "text/markdown", "prebuilt_path": artifact, "prebuilt_source_path": bundled.SourcePath})})
		save(&orm.DocumentProcessingState{DatasetID: previewDatasetID, DocumentID: task.DocID,
			ParseStatus: "succeeded", ChunkStatus: "succeeded", IndexStatus: "pending", UpdatedAt: now})
		wantedIDs = append(wantedIDs, remoteID)
		texts[task.DocID] = text
	}

	type reembedCall struct {
		prebuiltIndexAdmissionCall
		Groups         []string `json:"ng_names"`
		IdempotencyKey string   `json:"idempotency_key"`
		TaskIDs        []string `json:"-"`
	}
	var mu sync.Mutex
	var calls []reembedCall
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		origin := r.URL.Scheme + "://" + r.URL.Host
		if origin == previewOrigin && r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/latest.json":
				body, _ := json.Marshal(pin)
				return testJSONResponse(http.StatusOK, string(body)), nil
			case "/" + pin.ManifestPath:
				return testJSONResponse(http.StatusOK, string(manifest)), nil
			}
		}
		if origin == docOrigin && r.Method == http.MethodPost && r.URL.Path == "/v1/docs/reparse" {
			var call reembedCall
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				return nil, err
			}
			mu.Lock()
			for _, id := range call.DocumentIDs {
				call.TaskIDs = append(call.TaskIDs, fmt.Sprintf("fixture-reparse-%d-%s", len(calls)+1, id))
			}
			calls = append(calls, call)
			mu.Unlock()
			body, _ := json.Marshal(map[string]any{"task_ids": call.TaskIDs})
			return testJSONResponse(http.StatusOK, string(body)), nil
		}
		err := fmt.Errorf("unexpected outbound request %s %s%s", r.Method, r.URL.Host, r.URL.EscapedPath())
		t.Error(err)
		return nil, err
	})
	requests := func() []reembedCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
	checkReembed := func(call reembedCall) {
		t.Helper()
		activeModel, _ := call.Model["embed_main"].(map[string]any)
		if call.KbID != remoteKB || call.Level != doc.ProcessingLevelIndexed || call.Strategy != "reembed" ||
			!slices.Equal(slices.Sorted(slices.Values(call.Groups)), []string{"block", "line"}) ||
			activeModel["model"] != modelName || call.Model["_embedding_generation"] != float64(generation) ||
			strings.TrimSpace(call.IdempotencyKey) == "" {
			t.Fatal("requests must reembed the existing text groups in the original DocServer KB with the confirmed model")
		}
	}
	if ready, err := modelprovider.IsModelReady(t.Context(), db, previewUserID, "embed_main"); err != nil || !ready {
		t.Fatalf("the fixture model must be ready: %v", err)
	}
	config, err := modelconfig.LoadLLMConfig(t.Context(), db, previewUserID)
	embed, _ := config["embed_main"].(map[string]any)
	if err != nil || embed["model"] != modelName {
		t.Fatalf("the fixture model must be resolvable for DocServer: %v", err)
	}

	payload := installJobPayload{MarketItemID: previewItemID, UserID: previewUserID, Prebuilt: &pin, ProcessingLevel: doc.ProcessingLevelIndexed,
		IndexConfirmation: &installIndexConfirmation{Source: "own", ModelID: "batch-embed", Signature: signature, Generation: generation}}
	imported := &doc.MarketImportResult{DatasetID: previewDatasetID, TaskIDs: cfg.TaskIDs, Submitted: size}
	lease := time.Now().Add(time.Minute)
	parent := orm.AsyncJob{ID: "retry-indexed-install", JobType: installJobType, ResourceType: "knowledge_market_item", ResourceID: previewItemID,
		Status: "running", AttemptCount: 1, LockedBy: "fixture-install-worker", LockUntil: &lease, CreateUserID: previewUserID,
		PayloadJSON: previewJSON(t, payload), CreatedAt: now, UpdatedAt: now}
	save(&parent)
	attempt := asyncjob.Job{ID: parent.ID, JobType: installJobType, ResourceType: parent.ResourceType, ResourceID: parent.ResourceID,
		CreateUserID: previewUserID, AttemptCount: 1}
	result, err := completePrebuiltMarketInstall(t.Context(), db, attempt, payload, cfg, imported)
	if err != nil {
		t.Fatalf("register the original index dispatch: %v", err)
	}
	if err := db.Model(&parent).Updates(map[string]any{"status": "succeeded", "finished_at": now,
		"locked_by": "", "lock_until": nil, "result_json": result.ResultJSON}).Error; err != nil {
		t.Fatal(err)
	}
	var index orm.AsyncJob
	if err := db.Take(&index, "job_type = ? AND status = ?", doc.MarketIndexJobType, asyncjob.StatusPending).Error; err != nil {
		t.Fatalf("the confirmed install did not register its original index dispatch: %v", err)
	}

	RegisterAsyncJobs()
	ctx, cancel := context.WithCancel(t.Context())
	runner := asyncjob.Start(ctx, db, asyncjob.Options{Concurrency: 1, PollInterval: 10 * time.Millisecond})
	stop := sync.OnceFunc(func() { cancel(); <-runner.Done() })
	defer stop()
	waitSettled := func(id string) {
		t.Helper()
		var job orm.AsyncJob
		waitPrebuiltIndexAdmission(t, "the dispatch job and queue to settle", func() bool {
			if err := db.Take(&job, "id = ?", id).Error; err != nil {
				t.Fatal(err)
			}
			var active int64
			if err := db.Model(&orm.AsyncJob{}).Where("status IN ?", []string{"pending", "running"}).Count(&active).Error; err != nil {
				t.Fatal(err)
			}
			return active == 0 && (job.Status == "succeeded" || job.Status == "failed" || job.Status == "canceled")
		})
		if job.Status != "succeeded" {
			t.Fatalf("dispatch job %s did not succeed: status=%s error=%q", id, job.Status, job.ErrorMessage)
		}
	}
	waitSettled(index.ID)
	firstCalls := requests()
	if len(firstCalls) == 2 && len(firstCalls[0].DocumentIDs) == 1 {
		firstCalls[0], firstCalls[1] = firstCalls[1], firstCalls[0]
	}
	if len(firstCalls) != 2 || len(firstCalls[0].DocumentIDs) != 20 || len(firstCalls[1].DocumentIDs) != 1 {
		t.Fatalf("fixture requires the real first dispatch to accept a batch of 20 and a batch of 1, got %d requests", len(firstCalls))
	}
	firstIDs := append(slices.Clone(firstCalls[0].DocumentIDs), firstCalls[1].DocumentIDs...)
	if !slices.Equal(slices.Sorted(slices.Values(firstIDs)), slices.Sorted(slices.Values(wantedIDs))) {
		t.Fatalf("initial index dispatch did not reach all installed documents: %v", firstIDs)
	}
	successfulTasks := make([]orm.Task, 0, 20)
	successfulWorkerDocuments := make(map[string]string, 20)
	successfulArtifacts := make(map[string]string, 20)
	remoteTail := firstCalls[1].DocumentIDs[0]
	var tail orm.Document
	var tailReceipt readonlyorm.LazyLLMDocServiceTaskRow
	for batch, call := range firstCalls {
		checkReembed(call)
		for i, remoteID := range call.DocumentIDs {
			var document orm.Document
			if err := db.Take(&document, "dataset_id = ? AND lazyllm_doc_id = ? AND deleted_at IS NULL", previewDatasetID, remoteID).Error; err != nil {
				t.Fatal(err)
			}
			var task orm.Task
			if err := db.Take(&task, "dataset_id = ? AND doc_id = ? AND lazyllm_task_id = ? AND task_type = ?",
				previewDatasetID, document.ID, call.TaskIDs[i], doc.TaskTypeReparse).Error; err != nil {
				t.Fatalf("real Core did not bind the accepted index receipt: %v", err)
			}
			workerState, indexState := "SUCCESS", "succeeded"
			if batch == 1 {
				workerState, indexState, tail = "WAITING", "running", document
			} else {
				successfulTasks = append(successfulTasks, task)
				successfulWorkerDocuments[task.DocID] = remoteID
			}
			receipt := readonlyorm.LazyLLMDocServiceTaskRow{TaskType: "DOC_REPARSE", TaskID: task.LazyllmTaskID, DocID: remoteID,
				KbID: remoteKB, Status: workerState, CreatedAt: now, UpdatedAt: now}
			save(&receipt)
			observed, _, _, err := doc.ObserveDocumentProcessingDetails(t.Context(), db, db, document)
			if err != nil || observed.ParseStatus != "succeeded" || observed.ChunkStatus != "succeeded" || observed.IndexStatus != indexState {
				t.Fatalf("installed text %s must remain ready with authoritative Worker index status %s: %+v (%v)", document.ID, indexState, observed, err)
			}
			if batch == 1 {
				tailReceipt = receipt
			} else {
				if observed.IndexArtifactRef == "" {
					t.Fatalf("successful fixture document %s must have its existing index artifact binding", document.ID)
				}
				successfulArtifacts[document.ID] = observed.IndexArtifactRef
			}
		}
	}
	var readyText int64
	if err := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).Where("kb_id = ? AND task_type = 'DOC_ADD' AND status = 'SUCCESS'", remoteKB).
		Count(&readyText).Error; err != nil || readyText != size {
		t.Fatalf("expected all %d authoritative text import receipts successful: count=%d err=%v", size, readyText, err)
	}
	router.HandleFunc("/knowledge-market/tasks/{job_id}:retry", MarketRetryTask).Methods(http.MethodPost)
	retry := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/knowledge-market/tasks/"+parent.ID+":retry", nil)
		req.Header.Set("X-User-Id", previewUserID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	jobIDs := func() []string {
		t.Helper()
		var ids []string
		if err := db.Model(&orm.AsyncJob{}).Order("id").Pluck("id", &ids).Error; err != nil {
			t.Fatal(err)
		}
		return ids
	}
	beforeWaitingRetry := jobIDs()
	if response := retry(); response.Code != http.StatusConflict {
		t.Fatalf("a waiting index receipt must not be retried: HTTP %d %s", response.Code, response.Body.String())
	}
	if !slices.Equal(beforeWaitingRetry, jobIDs()) || len(requests()) != len(firstCalls) {
		t.Fatal("retry while the Worker receipt was waiting created a new job or sent a new reembed request")
	}
	updated := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).
		Where("id = ? AND kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_REPARSE' AND status = 'WAITING'",
			tailReceipt.ID, remoteKB, remoteTail, tailReceipt.TaskID).
		UpdateColumn("status", "FAILED")
	if updated.Error != nil || updated.RowsAffected != 1 {
		t.Fatalf("transition the same waiting index receipt to FAILED: rows=%d err=%v", updated.RowsAffected, updated.Error)
	}
	observed, _, _, err := doc.ObserveDocumentProcessingDetails(t.Context(), db, db, tail)
	if err != nil || observed.ParseStatus != "succeeded" || observed.ChunkStatus != "succeeded" || observed.IndexStatus != "failed" {
		t.Fatalf("Core must observe the tail's confirmed index failure while preserving its text: %+v (%v)", observed, err)
	}
	t.Log("The original dispatch succeeded; all 21 text imports and 20 index receipts succeeded, and the same tail receipt changed from WAITING to FAILED")

	response := retry()
	if response.Code != http.StatusOK {
		t.Fatalf("retrying the original install's one confirmed index failure must enqueue a new job: HTTP %d %s", response.Code, response.Body.String())
	}
	retryID, _ := mustData(t, response)["job_id"].(string)
	if retryID == "" || retryID == parent.ID || retryID == index.ID {
		t.Fatalf("index retry must register a new job, got %q", retryID)
	}
	waitSettled(retryID)
	stop()
	allCalls := requests()
	if len(allCalls) != 3 || !slices.Equal(allCalls[2].DocumentIDs, []string{remoteTail}) {
		t.Fatalf("expected exactly one additional reembed request for the failed remote document %q, got %d total requests", remoteTail, len(allCalls))
	}
	retried := allCalls[2]
	checkReembed(retried)
	if retried.IdempotencyKey == firstCalls[0].IdempotencyKey || retried.IdempotencyKey == firstCalls[1].IdempotencyKey {
		t.Fatal("a confirmed failure retry must have a fresh DocServer idempotency key")
	}
	var boundRetry orm.Task
	if err := db.Take(&boundRetry, "dataset_id = ? AND doc_id = ? AND lazyllm_task_id = ? AND task_type = ? AND deleted_at IS NULL",
		previewDatasetID, tail.ID, retried.TaskIDs[0], doc.TaskTypeReparse).Error; err != nil {
		t.Fatalf("the new DocServer receipt was not bound to the failed document: %v", err)
	}
	for _, original := range successfulTasks {
		var active []orm.Task
		if err := db.Where("dataset_id = ? AND doc_id = ? AND task_type = ? AND deleted_at IS NULL", previewDatasetID, original.DocID, doc.TaskTypeReparse).Find(&active).Error; err != nil || len(active) != 1 || active[0].ID != original.ID || active[0].LazyllmTaskID != original.LazyllmTaskID {
			t.Fatalf("retry changed the successful task binding for %s: %v", original.DocID, err)
		}
		var receipt readonlyorm.LazyLLMDocServiceTaskRow
		if err := db.Take(&receipt, "kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_REPARSE' AND status = 'SUCCESS'",
			remoteKB, successfulWorkerDocuments[original.DocID], original.LazyllmTaskID).Error; err != nil {
			t.Fatalf("retry changed the authoritative successful receipt for %s: %v", original.DocID, err)
		}
		var document orm.Document
		if err := db.Take(&document, "id = ? AND dataset_id = ? AND lazyllm_doc_id = ? AND deleted_at IS NULL",
			original.DocID, previewDatasetID, receipt.DocID).Error; err != nil {
			t.Fatalf("retry changed the successful document's binding: %v", err)
		}
		observed, _, _, err := doc.ObserveDocumentProcessingDetails(t.Context(), db, db, document)
		if err != nil || observed.ParseStatus != "succeeded" || observed.ChunkStatus != "succeeded" ||
			observed.IndexStatus != "succeeded" || observed.IndexArtifactRef != successfulArtifacts[document.ID] {
			t.Fatalf("retry changed the successful text or index state for %s: %+v (%v)", document.ID, observed, err)
		}
	}
	installed, err = loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
	if err != nil || installed == nil || installed.DatasetID != previewDatasetID || installed.InstalledVersion != pin.Version ||
		installed.InstallState != string(orm.InstallStateDone) {
		t.Fatalf("retry changed the installed text library: %v", err)
	}
	current := decodeInstallConfig(installed)
	if current.Prebuilt == nil || *current.Prebuilt != pin || !slices.Equal(current.TaskIDs, cfg.TaskIDs) {
		t.Fatal("retry changed the pinned release or the original successful text import tasks")
	}
	service, err := doc.NewDocumentService(doc.DocumentServiceDeps{DB: db, LazyDB: db})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{successfulTasks[0].DocID, tail.ID} {
		content, err := service.ReadDocumentContent(t.Context(), doc.DocumentContentRequest{DatasetID: previewDatasetID, DocumentID: id,
			UserID: previewUserID, Caller: doc.DatasetCatalogCaller{UserID: previewUserID}})
		if err != nil || content.Text != texts[id] {
			t.Fatalf("retry changed or hid the imported text for %s: %v", id, err)
		}
	}
}
