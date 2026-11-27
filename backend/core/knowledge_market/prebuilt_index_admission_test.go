package knowledge_market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	"lazymind/core/store"

	"gorm.io/gorm"
)

type prebuiltIndexAdmissionCall struct {
	DocumentIDs []string       `json:"doc_ids"`
	KbID        string         `json:"kb_id"`
	Strategy    string         `json:"strategy"`
	Level       string         `json:"processing_level"`
	Model       map[string]any `json:"llm_config"`
}

type prebuiltIndexAdmissionAdd struct {
	KbID  string                       `json:"kb_id"`
	Level string                       `json:"processing_level"`
	Model map[string]any               `json:"llm_config"`
	Items []prebuiltIndexAdmissionFile `json:"items"`
}

type prebuiltIndexAdmissionFile struct {
	ID       string         `json:"doc_id"`
	Path     string         `json:"file_path"`
	Metadata map[string]any `json:"metadata"`
}

func TestPrebuiltIndexAdmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		waitingText  bool
		textOnly     bool
		changedModel bool
	}{
		{name: "new indexed install waits for both Worker receipts and resumes automatically", waitingText: true},
		{name: "new default text install remains text only with a ready model", textOnly: true},
		{name: "new install keeps text when a same-name replacement makes confirmation stale", waitingText: true, changedModel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantSubmit := tc.waitingText && !tc.changedModel
			const itemID, userID, kbID = "local-prebuilt-demo", "admission-reader", "admission-kb"
			const sourceOrigin, docOrigin = "http://127.0.0.1:18117", "http://127.0.0.1:18118"
			const modelName, generation = "text-embedding-3-small", 7
			t.Setenv("LAZYMIND_UPLOAD_ROOT", t.TempDir())
			t.Setenv("LAZYMIND_READONLY_SCHEMA", "")
			t.Setenv("LAZYMIND_RAG_ENABLED", "true")
			t.Setenv("LAZYMIND_RUNTIME_MODE", "local")
			t.Setenv("LAZYMIND_DOCUMENT_SERVICE_URL", docOrigin)
			t.Setenv("LAZYMIND_ALGO_SERVICE_URL", docOrigin)
			db := newUpdateTestDB(t)
			if err := db.AutoMigrate(&orm.Task{}, &orm.DocumentProcessingState{}, &orm.EmbeddingIndexState{},
				&orm.UserModelProvider{}, &orm.UserModelProviderGroup{}, &orm.UserModelProviderGroupModel{}, &orm.UserSelectedModel{},
				&orm.UserReadingPolicy{}, &orm.RejectedDocumentPublication{},
				&readonlyorm.LazyLLMDocRow{}, &readonlyorm.LazyLLMKBDocRow{}, &readonlyorm.LazyLLMDocServiceTaskRow{}); err != nil {
				t.Fatal(err)
			}
			if err := SeedCatalog(t.Context(), db, writeCatalog(t, coexistenceCatalog)); err != nil {
				t.Fatal(err)
			}
			store.Init(db, db, nil)
			t.Cleanup(func() { store.Init(nil, nil, nil) })
			save := func(row any) {
				t.Helper()
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			owner := orm.BaseModel{CreateUserID: userID, CreatedAt: now, UpdatedAt: now}
			save(&orm.UserModelProvider{ID: "test-provider", Name: "OpenAI", BaseModel: owner})
			for _, suffix := range []string{"a", "b"} {
				groupID := "embedding-group-" + suffix
				save(&orm.UserModelProviderGroup{ID: groupID, UserModelProviderID: "test-provider", BaseURL: "https://fixture.invalid/v1",
					APIKey: "fixture-key", IsVerified: true, BaseModel: owner})
				save(&orm.UserModelProviderGroupModel{ID: "embed-" + suffix, UserModelProviderID: "test-provider", UserModelProviderGroupID: groupID,
					ProviderName: "OpenAI", Name: modelName, ModelType: "embed", Enabled: true, BaseModel: owner})
			}
			signature := "v1:4:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64)
			save(&orm.EmbeddingIndexState{Scope: "global", Phase: "ready", ActiveSource: "own",
				ActiveModelID: "embed-a", ActiveModelName: modelName, Signature: signature, Generation: generation, UpdatedAt: now})

			bundle, _ := prebuiltDemoFiles(t)
			var mu sync.Mutex
			var calls []prebuiltIndexAdmissionCall
			added := make(map[string]string)
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				origin := r.URL.Scheme + "://" + r.URL.Host
				if origin == sourceOrigin && r.Method == http.MethodGet {
					mu.Lock()
					isImport := len(added) == 0
					mu.Unlock()
					if data, ok := bundle[r.URL.Path]; ok && (isImport || !strings.HasPrefix(r.URL.Path, "/objects/")) {
						return testJSONResponse(http.StatusOK, string(data)), nil
					}
				}
				if origin == docOrigin {
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/v1/kbs":
						return testJSONResponse(http.StatusOK, `{"data":{"kb_id":"admission-kb"}}`), nil
					case r.Method == http.MethodGet && r.URL.Path == "/v1/kbs/"+kbID:
						return testJSONResponse(http.StatusOK, `{"data":{"kb_id":"admission-kb","owner_id":"admission-reader","algo_ids":["general_algo"]}}`), nil
					case r.Method == http.MethodGet && r.URL.Path == "/v1/algo/general_algo/groups":
						return testJSONResponse(http.StatusOK, `{"code":200,"data":[]}`), nil
					case r.Method == http.MethodPost && r.URL.Path == "/v1/docs/add":
						var request prebuiltIndexAdmissionAdd
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							return nil, err
						}
						if request.KbID != kbID || request.Level != doc.ProcessingLevelChunked || len(request.Model) != 0 || len(request.Items) == 0 {
							t.Error("first import must send only the existing text slices to the new KB without a model")
							return testJSONResponse(http.StatusBadRequest, "{}"), nil
						}
						items := make([]map[string]string, 0, len(request.Items))
						sources := make(map[string]string, len(request.Items))
						err := db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
							for _, item := range request.Items {
								coreID, _ := item.Metadata["core_document_id"].(string)
								source, _ := item.Metadata["source_path"].(string)
								artifact, _ := item.Metadata["prebuilt_path"].(string)
								if item.ID == "" || coreID == "" || artifact == "" || (source != "reading-room.md" && source != "tool-shelf.md") {
									return fmt.Errorf("DOC_ADD did not carry validated fixture file metadata")
								}
								taskID, status := "import-"+item.ID, "SUCCESS"
								if tc.waitingText && source == "tool-shelf.md" {
									status = "WAITING"
								}
								rawMetadata, _ := json.Marshal(item.Metadata)
								metadata := string(rawMetadata)
								for _, row := range []any{
									&readonlyorm.LazyLLMDocRow{DocID: item.ID, Filename: source, Path: item.Path, Meta: &metadata,
										UploadStatus: "SUCCESS", SourceType: "EXTERNAL", CreatedAt: now, UpdatedAt: now},
									&readonlyorm.LazyLLMKBDocRow{KbID: kbID, DocID: item.ID, CreatedAt: now, UpdatedAt: now},
									&readonlyorm.LazyLLMDocServiceTaskRow{TaskType: "DOC_ADD", TaskID: taskID, DocID: item.ID,
										KbID: kbID, Status: status, CreatedAt: now, UpdatedAt: now},
								} {
									if err := tx.Create(row).Error; err != nil {
										return err
									}
								}
								if tc.waitingText {
									// Deliberately put the cached Core projection ahead of the Worker.
									update := tx.Model(&orm.DocumentProcessingState{}).Where("document_id = ?", coreID).
										Updates(map[string]any{"parse_status": "succeeded", "chunk_status": "succeeded"})
									if update.Error != nil || update.RowsAffected != 1 {
										return fmt.Errorf("could not force stale success in the existing Core projection: %v", update.Error)
									}
								}
								sources[item.ID] = source
								items = append(items, map[string]string{"doc_id": item.ID, "task_id": taskID})
							}
							return nil
						})
						if err != nil {
							t.Errorf("DocServer fixture could not register actual import identities: %v", err)
							return nil, err
						}
						mu.Lock()
						for id, source := range sources {
							added[id] = source
						}
						mu.Unlock()
						body, _ := json.Marshal(map[string]any{"data": map[string]any{"items": items}})
						return testJSONResponse(http.StatusOK, string(body)), nil
					case r.Method == http.MethodPost && r.URL.Path == "/v1/docs/reparse":
						var request prebuiltIndexAdmissionCall
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							return nil, err
						}
						var unfinished int64
						if err := db.WithContext(r.Context()).Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).
							Where("kb_id = ? AND task_type = 'DOC_ADD' AND status <> 'SUCCESS'", kbID).Count(&unfinished).Error; err != nil {
							return nil, err
						}
						if unfinished != 0 {
							t.Error("Core sent indexed reembed before the real waiting DOC_ADD receipt succeeded")
						}
						mu.Lock()
						calls = append(calls, request)
						mu.Unlock()
						ids := make([]string, 0, len(request.DocumentIDs))
						for _, id := range request.DocumentIDs {
							ids = append(ids, "indexed-"+id)
						}
						body, _ := json.Marshal(map[string]any{"task_ids": ids})
						return testJSONResponse(http.StatusOK, string(body)), nil
					}
				}
				err := fmt.Errorf("unexpected external request %s %s%s", r.Method, r.URL.Host, r.URL.EscapedPath())
				t.Error(err)
				return nil, err
			})
			pin, err := readPrebuiltRelease(t.Context(), sourceOrigin+"/latest.json", itemID)
			if err != nil {
				t.Fatal(err)
			}
			payload := installJobPayload{MarketItemID: itemID, UserID: userID, Prebuilt: pin, ProcessingLevel: doc.ProcessingLevelChunked}
			if !tc.textOnly {
				payload.ProcessingLevel = doc.ProcessingLevelIndexed
				payload.IndexConfirmation = &installIndexConfirmation{Source: "own", ModelID: "embed-a", Signature: signature, Generation: generation}
			}
			queued, err := asyncjob.Enqueue(t.Context(), db, asyncjob.EnqueueRequest{JobType: installJobType, ResourceType: "knowledge_market_item",
				ResourceID: itemID, CreateUserID: userID, IdempotencyKey: "fixture-new-install", Payload: payload, MaxAttempts: 2})
			if err != nil {
				t.Fatal(err)
			}
			assertModelReady := func() {
				t.Helper()
				if ready, err := modelprovider.IsModelReady(t.Context(), db, userID, "embed_main"); err != nil || !ready {
					t.Fatalf("fixture embedding model must remain ready: ready=%t err=%v", ready, err)
				}
				llm, err := modelconfig.LoadLLMConfig(t.Context(), db, userID)
				embed, _ := llm["embed_main"].(map[string]any)
				if err != nil || embed["model"] != modelName {
					t.Fatalf("fixture must configure the existing Core submit path's embedding role: %v", err)
				}
			}
			assertModelReady()
			var datasetCount int64
			if err := db.Model(&orm.Dataset{}).Count(&datasetCount).Error; err != nil || datasetCount != 0 {
				t.Fatalf("test must start with an empty personal library inventory: %d, %v", datasetCount, err)
			}

			RegisterAsyncJobs()
			ctx, cancel := context.WithCancel(t.Context())
			runner := asyncjob.Start(ctx, db, asyncjob.Options{Concurrency: 1, PollInterval: 10 * time.Millisecond})
			stop := sync.OnceFunc(func() { cancel(); <-runner.Done() })
			defer stop()
			var result orm.AsyncJob
			loadJob := func() {
				t.Helper()
				if err := db.Take(&result, "id = ?", queued.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			var installed *orm.KnowledgeMarketInstall
			var documents []orm.Document
			waitPrebuiltIndexAdmission(t, "real text import to bind both DOC_ADD tasks", func() bool {
				loadJob()
				if result.Status == "failed" {
					t.Fatalf("real text import failed before its Core bindings were established: %s", result.ErrorMessage)
				}
				installed, err = loadMarketInstallRow(t.Context(), db, userID, itemID)
				if err != nil {
					t.Fatal(err)
				}
				if installed == nil || installed.DatasetID == "" {
					return false
				}
				if err := db.Where("dataset_id = ? AND lazyllm_doc_id <> '' AND deleted_at IS NULL", installed.DatasetID).Find(&documents).Error; err != nil {
					t.Fatal(err)
				}
				return len(documents) == 2
			})
			if result.AttemptCount == 0 {
				t.Fatal("the durable install job did not execute")
			}

			var waitingReceipt readonlyorm.LazyLLMDocServiceTaskRow
			if tc.waitingText {
				var states int64
				if err := db.Model(&orm.DocumentProcessingState{}).Where("dataset_id = ? AND parse_status = 'succeeded' AND chunk_status = 'succeeded'", installed.DatasetID).
					Count(&states).Error; err != nil || states != 2 {
					t.Fatalf("fixture must expose two stale Core successes: count=%d err=%v", states, err)
				}
				if err := db.Take(&waitingReceipt, "kb_id = ? AND task_type = 'DOC_ADD' AND status = 'WAITING'", kbID).Error; err != nil {
					t.Fatalf("fixture must leave one authoritative receipt waiting: %v", err)
				}
				var successes int64
				if err := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).Where("kb_id = ? AND task_type = 'DOC_ADD' AND status = 'SUCCESS'", kbID).
					Count(&successes).Error; err != nil || successes != 1 {
					t.Fatalf("fixture must leave one authoritative receipt successful: count=%d err=%v", successes, err)
				}
			}

			// Keep the actual waiting receipt through several polls after Core has
			// bound both texts. The transport also rejects any premature submit.
			assertNoSubmissions := func() {
				t.Helper()
				mu.Lock()
				defer mu.Unlock()
				if len(calls) != 0 {
					t.Fatalf("index admission made %d unauthorized submissions", len(calls))
				}
			}
			if tc.waitingText {
				for i := 0; i < 8; i++ {
					assertNoSubmissions()
					time.Sleep(20 * time.Millisecond)
				}
				receiptStillWaiting := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).
					Where("kb_id = ? AND doc_id = ? AND task_id = ? AND task_type = 'DOC_ADD' AND status = 'WAITING'", kbID, waitingReceipt.DocID, waitingReceipt.TaskID)
				if tc.changedModel {
					var before, after orm.EmbeddingIndexState
					if err := db.Take(&before, "scope = ?", "global").Error; err != nil || before.ActiveModelID != "embed-a" ||
						before.ActiveModelName != modelName || before.ActiveSource != "own" || before.Generation != generation || before.Signature != signature {
						t.Fatalf("the installed text must still be waiting under its confirmed model: state=%+v err=%v", before, err)
					}
					var pending int64
					if err := receiptStillWaiting.Session(&gorm.Session{}).Count(&pending).Error; err != nil || pending != 1 {
						t.Fatalf("the same DOC_ADD receipt must still be WAITING immediately before the model switch: count=%d err=%v", pending, err)
					}
					if err := db.Model(&orm.EmbeddingIndexState{}).Where("scope = ?", "global").UpdateColumn("active_model_id", "embed-b").Error; err != nil {
						t.Fatal(err)
					}
					if err := db.Take(&after, "scope = ?", "global").Error; err != nil || after.ActiveModelID != "embed-b" ||
						after.ActiveModelName != before.ActiveModelName || after.ActiveSource != before.ActiveSource ||
						after.Generation != before.Generation || after.Signature != before.Signature {
						t.Fatalf("the fixture must change only the active model ID: state=%+v err=%v", after, err)
					}
					assertModelReady()
					assertNoSubmissions()
					t.Log("The equally available model changed only its ID while the original DOC_ADD receipt was still WAITING")
				}
				if receipt := receiptStillWaiting.UpdateColumn("status", "SUCCESS"); receipt.Error != nil || receipt.RowsAffected != 1 {
					t.Fatalf("complete the original waiting DOC_ADD receipt: rows=%d err=%v", receipt.RowsAffected, receipt.Error)
				}
				loadJob()
				t.Logf("Both real imported documents are bound, and the former WAITING receipt is now SUCCESS; install job status=%s", result.Status)
			}
			if wantSubmit {
				waitPrebuiltIndexAdmission(t, "indexed reembed for both real imported documents after Worker success", func() bool {
					loadJob()
					mu.Lock()
					defer mu.Unlock()
					count := 0
					for _, call := range calls {
						count += len(call.DocumentIDs)
					}
					return count >= len(documents)
				})
			} else {
				waitPrebuiltIndexAdmission(t, "the install and any follow-up jobs to finish", func() bool {
					loadJob()
					var remaining int64
					if err := db.Model(&orm.AsyncJob{}).Where("status IN ?", []string{"pending", "running"}).Count(&remaining).Error; err != nil {
						t.Fatal(err)
					}
					return remaining == 0 && (result.Status == "succeeded" || result.Status == "failed")
				})
				assertNoSubmissions()
			}
			stop()
			var successes int64
			if err := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).Where("kb_id = ? AND task_type = 'DOC_ADD' AND status = 'SUCCESS'", kbID).
				Count(&successes).Error; err != nil || successes != 2 {
				t.Fatalf("expected both authoritative Worker receipts successful: count=%d err=%v", successes, err)
			}
			var submitted, documentIDs []string
			for _, document := range documents {
				documentIDs = append(documentIDs, document.LazyllmDocID)
			}
			for _, call := range calls {
				actualEmbed, _ := call.Model["embed_main"].(map[string]any)
				if call.KbID != kbID || call.Strategy != "reembed" || call.Level != doc.ProcessingLevelIndexed ||
					actualEmbed["model"] != modelName || call.Model["_embedding_generation"] != float64(generation) {
					t.Fatal("DocServer must receive indexed reembed for the same KB and confirmed model generation")
				}
				submitted = append(submitted, call.DocumentIDs...)
			}
			if wantSubmit {
				if !slices.Equal(slices.Sorted(slices.Values(submitted)), slices.Sorted(slices.Values(documentIDs))) {
					t.Fatalf("expected indexed reembed for exactly both imported documents, got %v", submitted)
				}
			} else {
				assertNoSubmissions()
			}
			var dataset orm.Dataset
			if err := db.Take(&dataset, "id = ? AND deleted_at IS NULL", installed.DatasetID).Error; err != nil || dataset.ProcessingLevel != doc.ProcessingLevelChunked {
				t.Fatalf("index intent changed the personal library's default text level: %v", err)
			}
			if !wantSubmit {
				current, err := loadMarketInstallRow(t.Context(), db, userID, itemID)
				if err != nil || current == nil || current.DatasetID != installed.DatasetID || current.InstalledVersion != pin.Version || current.InstallState != string(orm.InstallStateDone) {
					t.Fatalf("expected a readable text install with its pinned version: %v", err)
				}
				service, err := doc.NewDocumentService(doc.DocumentServiceDeps{DB: db, LazyDB: db})
				if err != nil {
					t.Fatal(err)
				}
				for _, document := range documents {
					source := added[document.LazyllmDocID]
					expected := map[string]string{"reading-room.md": "14:30", "tool-shelf.md": "16:40"}[source]
					content, err := service.ReadDocumentContent(t.Context(), doc.DocumentContentRequest{DatasetID: installed.DatasetID, DocumentID: document.ID,
						UserID: userID, Caller: doc.DatasetCatalogCaller{UserID: userID}})
					if err != nil || expected == "" || !strings.Contains(content.Text, expected) {
						t.Fatalf("installed document %s could not be read: %v", document.ID, err)
					}
				}
			}
		})
	}
}

func waitPrebuiltIndexAdmission(t *testing.T, state string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for !ready() {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", state)
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}
