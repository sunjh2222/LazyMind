package knowledge_market

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/doc"
)

func TestMarketIndexProgressUsesCurrentWorkerReceiptsWithoutChangingText(t *testing.T) {
	pin, _ := previewRelease(t, "v1", []prebuiltDocument{
		previewDocument("a", "v1", 100), previewDocument("b", "v1", 120),
	})
	pin.URL = previewOrigin + "/latest.json"
	db, router := previewMarket(t, pin)
	stubMarketWorkerHealth(t)
	var dataset orm.Dataset
	if err := db.Take(&dataset, "id = ?", previewDatasetID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&orm.DocumentProcessingState{}); err != nil {
		t.Fatal(err)
	}
	save := func(row any) {
		t.Helper()
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	const parentID, childID = "signed-install", "index-dispatch"
	now := time.Now().UTC()
	confirmation := installIndexConfirmation{Source: "own", ModelID: "fixture-model", Signature: "fixture-signed-model", Generation: 1}
	installed, err := loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
	if err != nil || installed == nil {
		t.Fatalf("load installed fixture: %v", err)
	}
	cfg := decodeInstallConfig(installed)
	cfg.IndexDispatchJobID = childID
	if err := db.Model(installed).Update("config", previewJSON(t, cfg)).Error; err != nil {
		t.Fatal(err)
	}
	parent := orm.AsyncJob{ID: parentID, JobType: installJobType, ResourceID: previewItemID, CreateUserID: previewUserID,
		Status: "succeeded", CreatedAt: now, PayloadJSON: previewJSON(t, installJobPayload{MarketItemID: previewItemID,
			UserID: previewUserID, Prebuilt: &pin, ProcessingLevel: doc.ProcessingLevelIndexed, IndexConfirmation: &confirmation}),
		ResultJSON: previewJSON(t, doc.MarketImportResult{DatasetID: previewDatasetID, TaskIDs: cfg.TaskIDs})}
	save(&parent)
	save(&orm.AsyncJob{ID: childID, JobType: doc.MarketIndexJobType, ResourceID: previewItemID, CreateUserID: previewUserID,
		Status: "succeeded", IdempotencyKey: prebuiltIndexJobKey(parentID), CreatedAt: now,
		PayloadJSON: previewJSON(t, prebuiltIndexJobPayload{InstallJobID: parentID, DatasetID: previewDatasetID,
			Release: pin, TaskIDs: cfg.TaskIDs, Confirmation: confirmation})})
	old := parent
	old.ID, old.CreatedAt = "older-install", now.Add(-time.Hour)
	save(&old)
	for i, importID := range cfg.TaskIDs {
		id := fmt.Sprintf("local-%c", 'a'+i)
		save(&orm.Document{ID: id, DatasetID: previewDatasetID, LazyllmDocID: id,
			ResourceVersion: 1, Ext: json.RawMessage(`{"prebuilt_path":"fixture"}`)})
		taskID := "index-" + id
		remoteID := "worker-" + id
		save(&orm.Task{ID: taskID, TaskType: string(doc.TaskTypeReparse), DatasetID: previewDatasetID, KbID: previewDatasetID,
			DocID: id, LazyllmTaskID: remoteID, BaseModel: orm.BaseModel{CreatedAt: now.Add(time.Second)},
			Ext: previewJSON(t, map[string]any{"processing_level": doc.ProcessingLevelIndexed, "processing_revision": dataset.ProcessingRevision, "market_index_origin": map[string]any{
				"install_job_id": parentID, "import_task_id": importID, "document_version": 1}})})
		save(&orm.DocumentProcessingState{DatasetID: previewDatasetID, DocumentID: id, IndexArtifactRef: "core-task:" + taskID})
		status := "WORKING"
		if i == 0 {
			status = "SUCCESS"
		}
		save(&readonlyorm.LazyLLMDocServiceTaskRow{TaskID: remoteID, KbID: previewDatasetID, DocID: id, TaskType: "DOC_REPARSE", Status: status})
	}
	// A newer receipt under another KB must not override the correctly bound status.
	save(&readonlyorm.LazyLLMDocServiceTaskRow{TaskID: "worker-local-b", KbID: "other-kb", DocID: "local-b",
		TaskType: "DOC_REPARSE", Status: "SUCCESS", UpdatedAt: now.Add(time.Minute)})
	type taskResponse struct {
		JobID        string                `json:"job_id"`
		DisplayState string                `json:"display_state"`
		Index        *InstallIndexProgress `json:"index_progress"`
		CanRetry     bool                  `json:"can_retry"`
	}
	check := func(want InstallIndexProgress, count int, retry bool) {
		t.Helper()
		w := performGetWithUser(t, router, "/knowledge-market/tasks/"+parentID, previewUserID)
		var detail struct{ Data taskResponse }
		if err := json.Unmarshal(w.Body.Bytes(), &detail); w.Code != http.StatusOK || err != nil || detail.Data.Index == nil ||
			*detail.Data.Index != want || detail.Data.DisplayState != "done" || detail.Data.CanRetry != retry {
			t.Fatalf("detail did not separate text from current index progress: HTTP %d %s", w.Code, w.Body.String())
		}
		w = performGetWithUser(t, router, "/knowledge-market/tasks?view=summary", previewUserID)
		var summary struct {
			Data struct {
				Items      []taskResponse `json:"items"`
				Unfinished int            `json:"unfinished_count"`
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &summary); w.Code != http.StatusOK || err != nil || len(summary.Data.Items) != 2 ||
			summary.Data.Unfinished != count || summary.Data.Items[0].JobID != parentID || summary.Data.Items[0].Index == nil ||
			*summary.Data.Items[0].Index != want || summary.Data.Items[1].Index != nil {
			t.Fatalf("summary included unrelated history or lost active work: HTTP %d %s", w.Code, w.Body.String())
		}
	}
	check(InstallIndexProgress{State: "running", Total: 2, Done: 1, Pending: 1}, 1, false)
	if err := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).Where("task_id = ? AND kb_id = ?", "worker-local-b", previewDatasetID).
		Update("status", "FAILED").Error; err != nil {
		t.Fatal(err)
	}
	check(InstallIndexProgress{State: "partial_failed", Total: 2, Done: 1, Failed: 1}, 0, true)
}
