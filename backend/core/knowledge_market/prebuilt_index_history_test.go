package knowledge_market

import (
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

func TestDeletingCompletedMarketInstallHistoryPreservesPendingIndexDispatch(t *testing.T) {
	testPrebuiltIndexAfterHistoryDeletion(t, true)
}

func TestDeletingFailedMarketInstallHistoryRejectsLateIndexDispatch(t *testing.T) {
	testPrebuiltIndexAfterHistoryDeletion(t, false)
}

func testPrebuiltIndexAfterHistoryDeletion(t *testing.T, completed bool) {
	t.Helper()
	t.Setenv("LAZYMIND_RAG_ENABLED", "true")
	t.Setenv("LAZYMIND_RUNTIME_MODE", "local")
	root := t.TempDir()
	t.Setenv("LAZYMIND_UPLOAD_ROOT", root)
	const docOrigin = "http://127.0.0.1:18118"
	t.Setenv("LAZYMIND_DOCUMENT_SERVICE_URL", docOrigin)
	t.Setenv("LAZYMIND_ALGO_SERVICE_URL", docOrigin)
	bundle, _ := prebuiltDemoFiles(t)
	var pin prebuiltRelease
	if err := json.Unmarshal(bundle["/latest.json"], &pin); err != nil {
		t.Fatal(err)
	}
	pin.URL = previewOrigin + "/latest.json"
	var manifest prebuiltManifest
	if err := json.Unmarshal(bundle["/"+pin.ManifestPath], &manifest); err != nil || len(manifest.Documents) != 2 {
		t.Fatalf("expected the two frozen documents: %v", err)
	}
	db, router := previewMarket(t, pin)
	if err := db.AutoMigrate(&orm.DocumentProcessingState{}, &orm.EmbeddingIndexState{}, &orm.UserModelProvider{},
		&orm.UserModelProviderGroup{}, &orm.UserModelProviderGroupModel{}, &orm.UserSelectedModel{}, &readonlyorm.LazyLLMDocRow{}); err != nil {
		t.Fatal(err)
	}
	save := func(row any) {
		t.Helper()
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	owner := orm.BaseModel{CreateUserID: previewUserID, CreatedAt: now, UpdatedAt: now}
	const modelName, generation = "text-embedding-3-small", 7
	signature := "v1:4:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64)
	save(&orm.UserModelProvider{ID: "history-provider", Name: "OpenAI", BaseModel: owner})
	save(&orm.UserModelProviderGroup{ID: "history-group", UserModelProviderID: "history-provider", BaseURL: "https://fixture.invalid/v1",
		APIKey: "fixture-key", IsVerified: true, BaseModel: owner})
	save(&orm.UserModelProviderGroupModel{ID: "history-embed", UserModelProviderID: "history-provider", UserModelProviderGroupID: "history-group",
		ProviderName: "OpenAI", Name: modelName, ModelType: "embed", Enabled: true, BaseModel: owner})
	save(&orm.EmbeddingIndexState{Scope: "global", Phase: "ready", ActiveSource: "own", ActiveModelID: "history-embed",
		ActiveModelName: modelName, Signature: signature, Generation: generation, UpdatedAt: now})
	loadInstall := func() *orm.KnowledgeMarketInstall {
		t.Helper()
		installed, err := loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
		if err != nil || installed == nil {
			t.Fatalf("could not load installed text library: %v", err)
		}
		return installed
	}
	cfg := decodeInstallConfig(loadInstall())
	if len(cfg.TaskIDs) != 2 {
		t.Fatal("the installed library must have two existing DOC_ADD tasks")
	}
	if err := db.Model(&orm.Dataset{}).Where("id = ?", previewDatasetID).UpdateColumn("ext",
		json.RawMessage(`{"algo_id":"general_algo","prebuilt_market":true}`)).Error; err != nil {
		t.Fatal(err)
	}
	var documentIDs []string
	wantText := make(map[string]string, len(manifest.Documents))
	for i, bundled := range manifest.Documents {
		var task orm.Task
		if err := db.Take(&task, "id = ?", cfg.TaskIDs[i]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&task).UpdateColumn("algo_id", "general_algo").Error; err != nil {
			t.Fatal(err)
		}
		object := bundle["/"+bundled.ObjectPath]
		text, _, err := decodePrebuiltObject(object, bundled)
		if err != nil {
			t.Fatal(err)
		}
		folder := filepath.Join(root, task.DocID)
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		original, artifact := filepath.Join(folder, bundled.SourcePath), filepath.Join(folder, "prebuilt.jsonl.gz")
		if err := os.WriteFile(original, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifact, object, 0o600); err != nil {
			t.Fatal(err)
		}
		save(&orm.Document{ID: task.DocID, LazyllmDocID: task.DocID, DatasetID: previewDatasetID, DisplayName: bundled.Title + ".md",
			BaseModel: owner, Ext: previewJSON(t, map[string]any{"stored_path": original, "original_filename": bundled.SourcePath,
				"content_type": "text/markdown", "prebuilt_path": artifact, "prebuilt_source_path": bundled.SourcePath})})
		save(&orm.DocumentProcessingState{DatasetID: previewDatasetID, DocumentID: task.DocID, ParseStatus: "succeeded", ChunkStatus: "succeeded", IndexStatus: "pending", UpdatedAt: now})
		documentIDs = append(documentIDs, task.DocID)
		wantText[task.DocID] = text
	}

	var mu sync.Mutex
	var calls []prebuiltIndexAdmissionCall
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		origin := r.URL.Scheme + "://" + r.URL.Host
		if origin == previewOrigin && r.Method == http.MethodGet && (r.URL.Path == "/latest.json" || r.URL.Path == "/"+pin.ManifestPath) {
			return testJSONResponse(200, string(bundle[r.URL.Path])), nil
		}
		if origin == docOrigin && r.Method == http.MethodPost && r.URL.Path == "/v1/docs/reparse" {
			var call prebuiltIndexAdmissionCall
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				return nil, err
			}
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
			return testJSONResponse(200, `{"task_ids":["history-index-a","history-index-b"]}`), nil
		}
		err := fmt.Errorf("unexpected outbound request %s %s%s", r.Method, r.URL.Host, r.URL.EscapedPath())
		t.Error(err)
		return nil, err
	})
	if ready, err := modelprovider.IsModelReady(t.Context(), db, previewUserID, "embed_main"); err != nil || !ready {
		t.Fatalf("the fixture model must be ready: %v", err)
	}
	model, err := modelconfig.LoadLLMConfig(t.Context(), db, previewUserID)
	embed, _ := model["embed_main"].(map[string]any)
	if err != nil || embed["model"] != modelName {
		t.Fatalf("the fixture model must be resolvable for DocServer: %v", err)
	}
	payload := installJobPayload{MarketItemID: previewItemID, UserID: previewUserID, Prebuilt: &pin, ProcessingLevel: doc.ProcessingLevelIndexed,
		IndexConfirmation: &installIndexConfirmation{Source: "own", ModelID: "history-embed", Signature: signature, Generation: generation}}
	result := &doc.MarketImportResult{DatasetID: previewDatasetID, TaskIDs: cfg.TaskIDs}
	lease := now.Add(time.Minute)
	parent := orm.AsyncJob{ID: "history-indexed-install", JobType: installJobType, ResourceType: "knowledge_market_item", ResourceID: previewItemID,
		Status: "running", AttemptCount: 1, LockedBy: "history-install-worker", LockUntil: &lease, CreateUserID: previewUserID,
		PayloadJSON: previewJSON(t, payload), CreatedAt: now, UpdatedAt: now}
	if !completed {
		lease = now.Add(-time.Minute)
		parent.Status, parent.LockedBy, parent.FinishedAt = "failed", "", &now
		parent.ResultJSON = prebuiltImportResult(result).ResultJSON
	}
	save(&parent)
	attempt := asyncjob.Job{ID: parent.ID, JobType: installJobType, ResourceType: parent.ResourceType, ResourceID: parent.ResourceID,
		CreateUserID: previewUserID, AttemptCount: 1}
	var index orm.AsyncJob
	if completed {
		out, err := completePrebuiltMarketInstall(t.Context(), db, attempt, payload, cfg, result)
		if err != nil {
			t.Fatalf("the running install with a live lease must register dispatch: %v", err)
		}
		if err := db.Model(&parent).Updates(map[string]any{"status": "succeeded", "finished_at": now,
			"locked_by": "", "lock_until": nil, "result_json": out.ResultJSON}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Take(&index, "job_type = ? AND status = ?", doc.MarketIndexJobType, "pending").Error; err != nil {
			t.Fatalf("install completion must register the pending dispatch: %v", err)
		}
	}

	router.HandleFunc("/knowledge-market/tasks/{job_id}", MarketDeleteTask).Methods(http.MethodDelete)
	request := httptest.NewRequest(http.MethodDelete, "/knowledge-market/tasks/"+parent.ID, nil)
	request.Header.Set("X-User-Id", previewUserID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if got := mustData(t, response)["job_id"]; got != parent.ID {
		t.Fatalf("deleted history ID = %v, want %s", got, parent.ID)
	}
	var found int64
	if err := db.Model(&orm.AsyncJob{}).Where("id = ?", parent.ID).Count(&found).Error; err != nil || found != 0 {
		t.Fatalf("install history was not deleted: matches=%d err=%v", found, err)
	}
	if !completed {
		if _, err := completePrebuiltMarketInstall(t.Context(), db, attempt, payload, cfg, result); err == nil {
			t.Error("the expired install attempt registered dispatch after its failed history was deleted")
		}
		if err := db.Model(&orm.AsyncJob{}).Where("job_type = ?", doc.MarketIndexJobType).Count(&found).Error; err != nil || found != 0 {
			t.Errorf("late install must create no index job: count=%d err=%v", found, err)
		}
	}
	RegisterAsyncJobs()
	ctx, cancel := context.WithCancel(t.Context())
	runner := asyncjob.Start(ctx, db, asyncjob.Options{Concurrency: 1, PollInterval: 10 * time.Millisecond})
	stop := sync.OnceFunc(func() { cancel(); <-runner.Done() })
	defer stop()
	if completed {
		waitPrebuiltIndexAdmission(t, "the original index dispatch to finish after deletion", func() bool {
			if err := db.Take(&index, "id = ?", index.ID).Error; err != nil {
				t.Fatal(err)
			}
			return index.Status == "succeeded" || index.Status == "failed" || index.Status == "canceled"
		})
	} else {
		waitPrebuiltIndexAdmission(t, "the queue to settle after the expired install attempt", func() bool {
			var active int64
			if err := db.Model(&orm.AsyncJob{}).Where("status IN ?", []string{"pending", "running"}).Count(&active).Error; err != nil {
				t.Fatal(err)
			}
			return active == 0
		})
	}
	stop()
	if completed {
		if index.Status != "succeeded" || len(calls) != 1 {
			t.Fatalf("pending index dispatch after history deletion: status=%s requests=%d error=%q", index.Status, len(calls), index.ErrorMessage)
		}
		call := calls[0]
		if call.KbID != previewDatasetID || call.Level != doc.ProcessingLevelIndexed || call.Strategy != "reembed" ||
			!slices.Equal(slices.Sorted(slices.Values(call.DocumentIDs)), slices.Sorted(slices.Values(documentIDs))) {
			t.Fatalf("index dispatch did not use the original two installed texts: kb=%s level=%s strategy=%s docs=%v", call.KbID, call.Level, call.Strategy, call.DocumentIDs)
		}
	} else if len(calls) != 0 {
		t.Errorf("the late expired install attempt made %d reembed requests", len(calls))
	}
	installed := loadInstall()
	current := decodeInstallConfig(installed)
	if installed.DatasetID != previewDatasetID || installed.InstalledVersion != pin.Version || installed.InstallState != string(orm.InstallStateDone) ||
		current.Prebuilt == nil || *current.Prebuilt != pin || !slices.Equal(current.TaskIDs, cfg.TaskIDs) {
		t.Fatal("removing task history changed the active installed release or its text tasks")
	}
	service, err := doc.NewDocumentService(doc.DocumentServiceDeps{DB: db, LazyDB: db})
	if err != nil {
		t.Fatal(err)
	}
	for _, documentID := range documentIDs {
		content, err := service.ReadDocumentContent(t.Context(), doc.DocumentContentRequest{DatasetID: previewDatasetID, DocumentID: documentID,
			UserID: previewUserID, Caller: doc.DatasetCatalogCaller{UserID: previewUserID}})
		if err != nil || content.Text != wantText[documentID] {
			t.Fatalf("installed document %s changed or could not be read: %v", documentID, err)
		}
	}
}

func TestPrebuiltMarketIndexEnqueueRequiresLiveInstallAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		attempt int
		expires time.Duration
	}{
		{name: "install is no longer running", status: "failed", attempt: 1, expires: time.Minute},
		{name: "another attempt owns the lease", status: "running", attempt: 2, expires: time.Minute},
		{name: "running attempt's lease expired", status: "running", attempt: 1, expires: -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newUpdateTestDB(t)
			pin, _ := previewRelease(t, "v1", []prebuiltDocument{previewDocument("one", "v1", 20)})
			pin.URL = previewOrigin + "/latest.json"
			payload := installJobPayload{MarketItemID: previewItemID, UserID: previewUserID, Prebuilt: &pin, ProcessingLevel: doc.ProcessingLevelIndexed,
				IndexConfirmation: &installIndexConfirmation{Source: "own", ModelID: "fixture-embed", Generation: 1,
					Signature: "v1:4:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64)}}
			result := &doc.MarketImportResult{DatasetID: previewDatasetID, TaskIDs: []string{"import-task"}}
			cfg := installConfig{Prebuilt: &pin, TaskIDs: result.TaskIDs}
			now := time.Now().UTC()
			lease := now.Add(tc.expires)
			parent := orm.AsyncJob{ID: "install-attempt", JobType: installJobType, ResourceType: "knowledge_market_item", ResourceID: previewItemID,
				Status: tc.status, AttemptCount: tc.attempt, LockedBy: "install-worker", LockUntil: &lease, CreateUserID: previewUserID,
				PayloadJSON: previewJSON(t, payload), CreatedAt: now, UpdatedAt: now}
			if err := db.Create(&parent).Error; err != nil {
				t.Fatal(err)
			}
			attempt := asyncjob.Job{ID: parent.ID, JobType: installJobType, ResourceType: parent.ResourceType, ResourceID: parent.ResourceID,
				CreateUserID: previewUserID, AttemptCount: 1}
			if _, err := completePrebuiltMarketInstall(t.Context(), db, attempt, payload, cfg, result); err == nil {
				t.Error("a noncurrent install attempt did not report a dispatch admission failure")
			}
			var count int64
			if err := db.Model(&orm.AsyncJob{}).Where("job_type = ?", doc.MarketIndexJobType).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("a noncurrent attempt enqueued an index job: count=%d err=%v", count, err)
			}
		})
	}
}
