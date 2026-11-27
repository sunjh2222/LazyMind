package knowledge_market

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

func TestPrebuiltMarketIndexDispatchBatchesTwentyOneImportedDocuments(t *testing.T) {
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
	db, _ := previewMarket(t, pin)
	if err := db.AutoMigrate(&orm.DocumentProcessingState{}, &orm.EmbeddingIndexState{}, &orm.UserModelProvider{},
		&orm.UserModelProviderGroup{}, &orm.UserModelProviderGroupModel{}, &orm.UserSelectedModel{}); err != nil {
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
	}

	type reembedCall struct {
		prebuiltIndexAdmissionCall
		Groups         []string `json:"ng_names"`
		IdempotencyKey string   `json:"idempotency_key"`
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
			calls = append(calls, call)
			mu.Unlock()
			ids := make([]string, len(call.DocumentIDs))
			for i, id := range call.DocumentIDs {
				ids[i] = "reembed-" + id
			}
			body, _ := json.Marshal(map[string]any{"task_ids": ids})
			return testJSONResponse(http.StatusOK, string(body)), nil
		}
		err := fmt.Errorf("unexpected outbound request %s %s%s", r.Method, r.URL.Host, r.URL.EscapedPath())
		t.Error(err)
		return nil, err
	})
	if ready, err := modelprovider.IsModelReady(t.Context(), db, previewUserID, "embed_main"); err != nil || !ready {
		t.Fatalf("fixture model must be ready: %v", err)
	}
	config, err := modelconfig.LoadLLMConfig(t.Context(), db, previewUserID)
	embed, _ := config["embed_main"].(map[string]any)
	if err != nil || embed["model"] != modelName {
		t.Fatalf("fixture model must resolve to the expected embedding role: %v", err)
	}

	payload := installJobPayload{MarketItemID: previewItemID, UserID: previewUserID, Prebuilt: &pin, ProcessingLevel: doc.ProcessingLevelIndexed,
		IndexConfirmation: &installIndexConfirmation{Source: "own", ModelID: "batch-embed", Signature: signature, Generation: generation}}
	imported := &doc.MarketImportResult{DatasetID: previewDatasetID, TaskIDs: cfg.TaskIDs, Submitted: size}
	lease := time.Now().Add(time.Minute)
	parent := orm.AsyncJob{ID: "batch-indexed-install", JobType: installJobType, ResourceType: "knowledge_market_item", ResourceID: previewItemID,
		Status: "running", AttemptCount: 1, LockedBy: "batch-install-worker", LockUntil: &lease, CreateUserID: previewUserID,
		PayloadJSON: previewJSON(t, payload), CreatedAt: now, UpdatedAt: now}
	save(&parent)
	attempt := asyncjob.Job{ID: parent.ID, JobType: installJobType, ResourceType: parent.ResourceType, ResourceID: parent.ResourceID,
		CreateUserID: previewUserID, AttemptCount: 1}
	result, err := completePrebuiltMarketInstall(t.Context(), db, attempt, payload, cfg, imported)
	if err != nil {
		t.Fatalf("register the real pending index dispatch: %v", err)
	}
	if err := db.Model(&parent).Updates(map[string]any{"status": "succeeded", "finished_at": now,
		"locked_by": "", "lock_until": nil, "result_json": result.ResultJSON}).Error; err != nil {
		t.Fatal(err)
	}
	var index orm.AsyncJob
	if err := db.Take(&index, "job_type = ? AND status = ?", doc.MarketIndexJobType, asyncjob.StatusPending).Error; err != nil {
		t.Fatalf("the confirmed install did not register an index dispatch: %v", err)
	}
	RegisterAsyncJobs()
	ctx, cancel := context.WithCancel(t.Context())
	runner := asyncjob.Start(ctx, db, asyncjob.Options{Concurrency: 1, PollInterval: 10 * time.Millisecond})
	stop := sync.OnceFunc(func() { cancel(); <-runner.Done() })
	defer stop()
	waitPrebuiltIndexAdmission(t, "the dispatched job and its queue to settle", func() bool {
		if err := db.Take(&index, "id = ?", index.ID).Error; err != nil {
			t.Fatal(err)
		}
		var active int64
		if err := db.Model(&orm.AsyncJob{}).Where("status IN ?", []string{"pending", "running"}).Count(&active).Error; err != nil {
			t.Fatal(err)
		}
		return active == 0 && (index.Status == "succeeded" || index.Status == "failed" || index.Status == "canceled")
	})
	stop()
	if index.Status != "succeeded" {
		t.Fatalf("index dispatch did not succeed: status=%s error=%q", index.Status, index.ErrorMessage)
	}
	var receivedIDs []string
	var batchSizes []int
	var keys []string
	for _, call := range calls {
		actualModel, _ := call.Model["embed_main"].(map[string]any)
		if call.KbID != remoteKB || call.Level != doc.ProcessingLevelIndexed || call.Strategy != "reembed" ||
			actualModel["model"] != modelName || call.Model["_embedding_generation"] != float64(generation) {
			t.Fatalf("DocServer must receive indexed reembed for remote KB %q and the confirmed model generation, got KB %q", remoteKB, call.KbID)
		}
		if !slices.Equal(slices.Sorted(slices.Values(call.Groups)), []string{"block", "line"}) {
			t.Fatalf("DocServer must reembed both existing text groups, got %v", call.Groups)
		}
		receivedIDs = append(receivedIDs, call.DocumentIDs...)
		batchSizes = append(batchSizes, len(call.DocumentIDs))
		keys = append(keys, strings.TrimSpace(call.IdempotencyKey))
	}
	if !slices.Equal(slices.Sorted(slices.Values(receivedIDs)), slices.Sorted(slices.Values(wantedIDs))) {
		t.Fatalf("expected every existing remote document exactly once: got %v", receivedIDs)
	}
	if !slices.Equal(slices.Sorted(slices.Values(batchSizes)), []int{1, 20}) {
		t.Fatalf("all %d existing documents reached DocServer; want exactly two reembed requests of sizes 20 and 1, got %v", size, batchSizes)
	}
	if keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Fatal("each reembed batch must carry its own nonempty idempotency key")
	}
}
