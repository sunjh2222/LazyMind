package doc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"
	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

func TestRecoverPrebuiltMarketIndexReceiptsRequiresTheOriginalAcceptedBatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		want [2]string
	}{
		{name: "completed", want: [2]string{"worker-z", "worker-a"}},
		{name: "missing record"},
		{name: "unfinished record"},
		{name: "wrong endpoint"},
		{name: "wrong document", want: [2]string{"", "worker-a"}},
		{name: "wrong knowledge base", want: [2]string{"", "worker-a"}},
		{name: "wrong task type", want: [2]string{"", "worker-a"}},
		{name: "old artifact", want: [2]string{"", "worker-a"}},
		{name: "prepared but never sent"},
		{name: "lease lost while recording send authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_DB_DRIVER", "sqlite")
			t.Setenv("LAZYMIND_READONLY_SCHEMA", "")
			core := orm.MigrateTestDB(t, &orm.Dataset{}, &orm.Document{}, &orm.Task{}, &orm.DocumentProcessingState{})
			worker := orm.MigrateTestDB(t, &readonlyorm.LazyLLMDocServiceTaskRow{})
			store.Init(core.DB, worker.DB, nil)
			t.Cleanup(func() { store.Init(nil, nil, nil) })
			if err := worker.Exec(`CREATE TABLE lazyllm_idempotency_records (
				id INTEGER PRIMARY KEY, endpoint TEXT NOT NULL, idempotency_key TEXT NOT NULL, req_hash TEXT NOT NULL,
				status TEXT NOT NULL, response_json TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, UNIQUE(endpoint, idempotency_key))`).Error; err != nil {
				t.Fatal(err)
			}
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				err := fmt.Errorf("receipt recovery must not send HTTP: %s %s", r.Method, r.URL.Path)
				t.Error(err)
				return nil, err
			})

			save := func(db *gorm.DB, row any) {
				t.Helper()
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			const datasetID, kbID, installID = "installed", "docserver-kb", "install-job"
			const revision, version = 3, 2
			now := time.Now().UTC()
			owner := orm.BaseModel{CreateUserID: "reader", CreatedAt: now, UpdatedAt: now}
			save(core.DB, &orm.Dataset{ID: datasetID, KbID: kbID, ProcessingLevel: ProcessingLevelChunked,
				ProcessingRevision: revision, Ext: json.RawMessage(`{"prebuilt_market":true}`), BaseModel: owner})
			var imports, tasks []orm.Task
			for _, suffix := range []string{"z", "a"} {
				coreID, externalID := "document-"+suffix, "remote-"+suffix
				importID, taskID := "import-"+suffix, "index-"+suffix
				save(core.DB, &orm.Document{ID: coreID, DatasetID: datasetID, LazyllmDocID: externalID,
					ResourceVersion: version, BaseModel: owner, Ext: json.RawMessage(`{"prebuilt_path":"fixture.jsonl.gz"}`)})
				imported := orm.Task{ID: importID, DocID: coreID, KbID: kbID, DatasetID: datasetID,
					TaskType: string(TaskTypeParseUploaded), LazyllmTaskID: "added-" + suffix, BaseModel: owner}
				save(core.DB, &imported)
				imports = append(imports, imported)
				task := orm.Task{ID: taskID, DocID: coreID, KbID: kbID, DatasetID: datasetID, TaskType: string(TaskTypeReparse), BaseModel: owner,
					Ext: mustJSON(taskExt{ProcessingLevel: ProcessingLevelIndexed, ProcessingRevision: revision,
						MarketIndexOrigin: &marketIndexTaskOrigin{InstallJobID: installID, ImportTaskID: importID, DocumentVersion: version}})}
				task.CreatedAt = now.Add(time.Second)
				save(core.DB, &task)
				tasks = append(tasks, task)
				state := newDocumentProcessingState(datasetID, coreID, now)
				state.ParseStatus, state.ChunkStatus = "succeeded", "succeeded"
				state.IndexArtifactRef = "core-task:" + taskID
				save(core.DB, &state)
			}
			if err := core.Transaction(func(tx *gorm.DB) error { return saveMarketIndexBatch(tx, datasetID, tasks) }); err != nil {
				t.Fatal(err)
			}
			lostLease := tc.name == "lease lost while recording send authorization"
			started := tc.name != "prepared but never sent" && !lostLease
			if started {
				if err := startMarketIndexBatch(t.Context(), core.DB, datasetID, tasks, func(*gorm.DB) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if lostLease {
				var checks int
				expired := fmt.Errorf("fixture lease expired")
				err := startMarketIndexBatch(t.Context(), core.DB, datasetID, tasks, func(tx *gorm.DB) error {
					checks++
					if checks == 1 {
						return nil
					}
					var current orm.Task
					var ext taskExt
					if err := tx.Take(&current, "id = ?", tasks[0].ID).Error; err != nil {
						return err
					}
					if err := json.Unmarshal(current.Ext, &ext); err != nil || ext.MarketIndexBatch == nil || !ext.MarketIndexBatch.Started {
						return fmt.Errorf("expected the staged send marker to exist inside the transaction: %v", err)
					}
					return expired
				})
				if err != expired {
					t.Fatalf("send authorization must roll back when the final lease check fails: %v", err)
				}
			}
			key := parseSubmissionKey(datasetID, tasks)
			if tc.name != "missing record" {
				endpoint, status := "/v1/docs/reparse", "COMPLETED"
				if tc.name == "unfinished record" {
					status = "PROCESSING"
				}
				if tc.name == "wrong endpoint" {
					endpoint = "/v1/docs/add"
				}
				if err := worker.Exec(`INSERT INTO lazyllm_idempotency_records (endpoint,idempotency_key,req_hash,status,response_json)
					VALUES (?,?,?,?,?)`, endpoint, key, "fixture", status, `{"task_ids":["worker-z","worker-a"]}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, suffix := range []string{"z", "a"} {
				receipt := readonlyorm.LazyLLMDocServiceTaskRow{TaskType: "DOC_REPARSE", TaskID: "worker-" + suffix,
					DocID: "remote-" + suffix, KbID: kbID, Status: "FAILED", CreatedAt: now, UpdatedAt: now}
				if suffix == "z" {
					switch tc.name {
					case "wrong document":
						receipt.DocID = "remote-a"
					case "wrong knowledge base":
						receipt.KbID = "another-kb"
					case "wrong task type":
						receipt.TaskType = "DOC_ADD"
					}
				}
				save(worker.DB, &receipt)
			}
			if tc.name == "old artifact" {
				if err := core.Model(&orm.DocumentProcessingState{}).Where("document_id = ?", imports[0].DocID).
					UpdateColumn("index_artifact_ref", "core-task:replacement").Error; err != nil {
					t.Fatal(err)
				}
			}
			wantCount := 0
			for _, id := range tc.want {
				if id != "" {
					wantCount++
				}
			}
			// Read the imports in reverse submission order: only persisted positions identify the correct receipt.
			for run, want := range []int{wantCount, 0} {
				recovered, err := RecoverPrebuiltMarketIndexReceipts(t.Context(), core.DB, datasetID, installID, []orm.Task{imports[1], imports[0]})
				if err != nil || recovered != want {
					t.Fatalf("run %d recovered=%d, want %d: %v", run+1, recovered, want, err)
				}
				for i, task := range tasks {
					var current orm.Task
					if err := core.Take(&current, "id = ?", task.ID).Error; err != nil || current.LazyllmTaskID != tc.want[i] {
						t.Fatalf("task %s bound %q, want %q: %v", task.ID, current.LazyllmTaskID, tc.want[i], err)
					}
					var ext taskExt
					if err := json.Unmarshal(current.Ext, &ext); err != nil || ext.MarketIndexBatch == nil || ext.MarketIndexBatch.Started != started {
						t.Fatalf("task %s send marker changed or escaped rollback: %v", task.ID, err)
					}
				}
			}
			var count int64
			if err := core.Model(&orm.Task{}).Where("task_type = ?", TaskTypeReparse).Count(&count).Error; err != nil || count != int64(len(tasks)) {
				t.Fatalf("recovery must reuse the original Core tasks: count=%d err=%v", count, err)
			}
		})
	}
}
