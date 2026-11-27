package knowledge_market

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

func TestMarketRetryRecoversExhaustedPrebuiltSubmission(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		jobStatus  string
		prebuilt   bool
		canRetry   bool
		wantStatus int
	}{
		{"exhausted prebuilt attempt", "failed", true, true, http.StatusOK},
		{"still running prebuilt attempt", "running", true, false, http.StatusConflict},
		{"unconfirmed original-source attempt", "failed", false, false, http.StatusConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const owner, datasetID, jobID = "user-a", "personal-library", "submission-job"
			const uncertainTask, completedTask, previousExternal = "file-unconfirmed", "file-completed", "earlier-failed-worker"
			const logicalAttempt = "88f0c577-24f2-484e-b299-1bdb53cb963f"

			router := newTaskTestRouter(t)
			router.HandleFunc("/knowledge-market/tasks/{job_id}:retry", MarketRetryTask).Methods(http.MethodPost)
			router.HandleFunc("/knowledge-market/tasks/{job_id}/files", MarketTaskFiles).Methods(http.MethodGet)
			db := store.DB()
			t.Cleanup(func() { store.Init(nil, nil, nil) })
			now := time.Now().UTC()
			payload := installJobPayload{MarketItemID: "law-cn", UserID: owner, OriginalSource: !scenario.prebuilt}
			if scenario.prebuilt {
				if err := SeedCatalog(context.Background(), db, writeCatalog(t, coexistenceCatalog)); err != nil {
					t.Fatal(err)
				}
				files, _ := prebuiltDemoFiles(t)
				var release prebuiltRelease
				if err := json.Unmarshal(files["/latest.json"], &release); err != nil {
					t.Fatal(err)
				}
				release.URL = "http://127.0.0.1:18117/latest.json"
				payload.MarketItemID = release.ItemID
				payload.Prebuilt = &release
				payload.PrebuiltRetryAttempt = logicalAttempt
				payload.PrebuiltRetryTasks = []string{uncertainTask, completedTask}
			}
			cfg, err := json.Marshal(installConfig{Prebuilt: payload.Prebuilt, TaskIDs: []string{uncertainTask, completedTask}})
			if err != nil {
				t.Fatal(err)
			}
			payloadJSON, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&orm.Dataset{
				ID: datasetID, KbID: datasetID, DisplayName: "Personal library",
				ProcessingLevel: "chunked", Ext: json.RawMessage(`{}`),
				BaseModel: orm.BaseModel{CreateUserID: owner, CreatedAt: now, UpdatedAt: now},
			}).Error; err != nil {
				t.Fatal(err)
			}
			insertInstallWithConfig(t, db, payload.MarketItemID, owner, "done", datasetID, string(cfg), now)
			insertLinkedTask(t, db, uncertainTask, "doc-first", previousExternal, datasetID)
			insertDocServiceTask(t, db, previousExternal, "doc-first", datasetID, "FAILED")
			retrying := map[string]any{
				"market_submission": "retrying", "market_previous_task_id": previousExternal,
				"task_type": "TASK_TYPE_PARSE_UPLOADED", "task_state": "FAILED", "data_source_type": "MARKET",
			}
			if scenario.prebuilt {
				retrying["market_retry_attempt"] = logicalAttempt
			}
			ext, err := json.Marshal(retrying)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&orm.Task{}).Where("id = ?", uncertainTask).Update("ext", json.RawMessage(ext)).Error; err != nil {
				t.Fatal(err)
			}
			insertLinkedTask(t, db, completedTask, "doc-second", "current-successful-worker", datasetID)
			insertDocServiceTask(t, db, "current-successful-worker", "doc-second", datasetID, "SUCCESS")
			if scenario.prebuilt {
				acknowledged, err := json.Marshal(map[string]string{
					"market_retry_attempt": logicalAttempt, "market_submission": "submitted",
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&orm.Task{}).Where("id = ?", completedTask).Update("ext", json.RawMessage(acknowledged)).Error; err != nil {
					t.Fatal(err)
				}
			}

			resultJSON, err := json.Marshal(map[string]any{
				"dataset_id": datasetID, "task_ids": []string{uncertainTask, completedTask},
			})
			if err != nil {
				t.Fatal(err)
			}
			job := orm.AsyncJob{
				ID: jobID, JobType: installJobType, Status: scenario.jobStatus,
				ResourceType: "knowledge_market_item", ResourceID: payload.MarketItemID,
				IdempotencyKey: "kb_retry:" + payload.MarketItemID + ":" + owner,
				PayloadJSON:    json.RawMessage(payloadJSON), ResultJSON: json.RawMessage(resultJSON),
				AttemptCount: 2, MaxAttempts: 2, NextRunAt: now,
				CreateUserID: owner, CreatedAt: now, UpdatedAt: now,
			}
			if job.Status == "failed" {
				job.FinishedAt = &now
			}
			if err := db.Create(&job).Error; err != nil {
				t.Fatal(err)
			}

			detail := mustTaskData(t, performGetWithUser(t, router, "/knowledge-market/tasks/"+jobID, owner))
			parse, ok := detail["parse"].(map[string]any)
			if !ok || parse["state"] != "unknown" || parse["unknown"] != float64(1) || parse["done"] != float64(1) {
				t.Fatalf("fixture did not reach an unacknowledged submission with one successful sibling: %v", detail)
			}
			if got := detail["can_retry"]; got != scenario.canRetry {
				t.Errorf("GET can_retry=%v, want %v", got, scenario.canRetry)
			}
			if scenario.canRetry && (detail["can_cancel"] != false || detail["can_delete"] != false) {
				t.Fatalf("a replayable submission must still block cancellation and deletion: %v", detail)
			}
			post := performPost(t, router, "/knowledge-market/tasks/"+jobID+":retry", owner)
			if post.Code != scenario.wantStatus {
				t.Fatalf("POST :retry status=%d, want %d, body=%s", post.Code, scenario.wantStatus, post.Body.String())
			}
			if !scenario.canRetry {
				return
			}
			accepted := mustTaskData(t, post)
			id, _ := accepted["job_id"].(string)
			var resumed orm.AsyncJob
			if id == "" || id == jobID {
				t.Fatalf("retry did not queue another async job: %v", accepted)
			}
			if err := db.Where("id = ?", id).Take(&resumed).Error; err != nil {
				t.Fatal(err)
			}
			var replay installJobPayload
			if err := json.Unmarshal(resumed.PayloadJSON, &replay); err != nil {
				t.Fatal(err)
			}
			if resumed.Status != "pending" || replay.Prebuilt == nil || *replay.Prebuilt != *payload.Prebuilt ||
				replay.PrebuiltRetryAttempt != logicalAttempt || !slices.Equal(replay.PrebuiltRetryTasks, payload.PrebuiltRetryTasks) {
				t.Fatalf("retry lost its pinned release or changed its logical submission: %s", resumed.PayloadJSON)
			}
			if err := db.Where("id = ?", jobID).Take(&job).Error; err != nil {
				t.Fatal(err)
			}
			var preserved struct {
				Parse *parseProgressInfo `json:"parse"`
			}
			if err := json.Unmarshal(job.ResultJSON, &preserved); err != nil || preserved.Parse == nil || preserved.Parse.State != "unknown" {
				t.Fatalf("the predecessor must preserve its unconfirmed file snapshot: %s", job.ResultJSON)
			}
			historic := mustTaskData(t, performGetWithUser(t, router, "/knowledge-market/tasks/"+jobID, owner))
			if historic["can_retry"] != false {
				t.Fatalf("only the latest job may offer a retry: %v", historic)
			}
			if stale := performPost(t, router, "/knowledge-market/tasks/"+jobID+":retry", owner); stale.Code != http.StatusConflict {
				t.Fatalf("replaying a superseded job got %d, want 409", stale.Code)
			}

			countUnfinished := func(stage string, want int) {
				t.Helper()
				summary := mustTaskData(t, performGetWithUser(t, router, "/knowledge-market/tasks?view=summary", owner))
				if got := summary["unfinished_count"]; got != float64(want) {
					t.Errorf("%s: unfinished_count=%v, want %d", stage, got, want)
				} else {
					t.Logf("%s: unfinished_count=%d", stage, want)
				}
			}
			countUnfinished("replay queued", 1)

			const acceptedExternal = "worker-confirmed-after-replay"
			insertDocServiceTask(t, db, acceptedExternal, "doc-first", datasetID, "PARSING")
			confirmed, err := json.Marshal(map[string]string{
				"market_retry_attempt": logicalAttempt, "market_submission": "submitted",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&orm.Task{}).Where("id = ?", uncertainTask).Updates(map[string]any{
				"ext": json.RawMessage(confirmed), "lazyllm_task_id": acceptedExternal,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&resumed).Updates(map[string]any{
				"status": "succeeded", "result_json": json.RawMessage(resultJSON),
				"attempt_count": 1, "finished_at": time.Now().UTC(),
			}).Error; err != nil {
				t.Fatal(err)
			}
			countUnfinished("replay parsing", 1)

			taskRow := readonlyorm.LazyLLMDocServiceTaskRow{}
			if err := db.Table(taskRow.TableName()).Where("task_id = ?", acceptedExternal).Update("status", "SUCCESS").Error; err != nil {
				t.Fatal(err)
			}
			latest := mustTaskData(t, performGetWithUser(t, router, "/knowledge-market/tasks/"+resumed.ID, owner))
			latestParse, ok := latest["parse"].(map[string]any)
			if !ok || latestParse["state"] != "done" || latestParse["done"] != float64(2) {
				t.Fatalf("fixture did not complete the replay: %v", latest)
			}
			countUnfinished("replay completed", 0)

			history := mustTaskData(t, performGetWithUser(t, router, "/knowledge-market/tasks/"+jobID+"/files?status=unfinished", owner))
			items, ok := history["items"].([]any)
			if !ok || len(items) != 1 || history["file_details_available"] != true || history["total"] != float64(1) {
				t.Fatalf("old task no longer exposes its preserved unfinished file: %v", history)
			}
			oldFile, ok := items[0].(map[string]any)
			if !ok || oldFile["file_id"] != uncertainTask || oldFile["status"] != "unfinished" || oldFile["reason_code"] != "unknown" {
				t.Fatalf("old file history must remain unknown after its replay completes: %v", oldFile)
			}
		})
	}
}
