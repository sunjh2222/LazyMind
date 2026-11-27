package doc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
)

func TestPrebuiltRetryReplaysLostResponseWithSameIdentity(t *testing.T) {
	db := newDocumentTestDB(t)
	ctx := context.Background()
	t.Setenv("LAZYMIND_UPLOAD_ROOT", t.TempDir())
	t.Setenv("LAZYMIND_DOCUMENT_SERVICE_URL", "http://doc-server.test")
	t.Setenv("LAZYMIND_RAG_ENABLED", "true")
	seedDocumentListDataset(t, db, "library", "reader")
	var ds orm.Dataset
	if err := db.Take(&ds, "id = ?", "library").Error; err != nil {
		t.Fatal(err)
	}
	ds.ProcessingLevel = ProcessingLevelChunked
	ds.Ext = json.RawMessage(`{"prebuilt_market":true,"algo_id":"general_algo"}`)
	if err := db.Save(&ds).Error; err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "derived.md")
	if err := os.WriteFile(source, []byte("# Derived text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	initial, _, err := registerMarketFile(ctx, db.DB, &ds, "reader", "", MarketImportFile{
		LocalPath: source, DisplayName: "Source.md",
		PrebuiltPath:     "../knowledge_market/testdata/local_prebuilt_demo/objects/2181751addc837649f7bddf8d0368233cff49d71f698fae5e1d986a962f6cddf.jsonl.gz",
		PrebuiltIdentity: "release:source", SourcePath: "source.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	var task orm.Task
	if err := db.Take(&task, "id = ?", initial).Error; err != nil {
		t.Fatal(err)
	}
	var document orm.Document
	if err := db.Take(&document, "id = ?", task.DocID).Error; err != nil {
		t.Fatal(err)
	}
	var ext documentExt
	if err := json.Unmarshal(document.Ext, &ext); err != nil {
		t.Fatal(err)
	}
	if ext.ContentType != "text/markdown; charset=utf-8" {
		t.Fatalf("derived Markdown media type = %q", ext.ContentType)
	}
	if err := db.Model(&task).Update("lazyllm_task_id", "worker-failed").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&readonlyorm.LazyLLMDocServiceTaskRow{
		TaskID: "worker-failed", DocID: task.DocID, KbID: ds.KbID, Status: "FAILED",
	}).Error; err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	var first []byte
	calls := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var sent addRequest
		if err := json.Unmarshal(data, &sent); err != nil {
			t.Fatal(err)
		}
		if sent.IdempotencyKey == "" || sent.IdempotencyKey == parseSubmissionKey(ds.ID, []orm.Task{task}) ||
			len(sent.Items) != 1 || sent.Items[0].DocID != task.DocID {
			t.Fatalf("new submission did not keep the document and change the request identity: %+v", sent)
		}
		if calls == 1 {
			first = data
			return nil, io.ErrUnexpectedEOF
		}
		if !bytes.Equal(first, data) {
			t.Fatalf("unacknowledged retry changed the request: first=%s latest=%s", first, data)
		}
		result, _ := json.Marshal(map[string]any{"items": []map[string]any{{
			"task_id": "worker-retry", "doc_id": task.DocID, "accepted": true,
			"metadata": sent.Items[0].Metadata,
		}}})
		return testJSONResponse(http.StatusOK, string(result)), nil
	})

	retry := func() (int, error) {
		return RetryPrebuiltMarketTasks(ctx, ds.ID, "reader", "", "logical-retry-id", []string{initial})
	}
	if count, err := retry(); count != 0 || err == nil {
		t.Fatalf("lost response unexpectedly acknowledged: count=%d err=%v", count, err)
	}
	if count, err := retry(); count != 1 || err != nil {
		t.Fatalf("retry could not bind the recovered response: count=%d err=%v", count, err)
	}
	if count, err := retry(); count != 1 || err != nil || calls != 2 {
		t.Fatalf("acknowledged retry was not reused: count=%d calls=%d err=%v", count, calls, err)
	}
	if err := db.Take(&task, "id = ?", initial).Error; err != nil {
		t.Fatal(err)
	}
	if task.LazyllmTaskID != "worker-retry" {
		t.Fatalf("Core retained the old failed worker binding: %s", task.LazyllmTaskID)
	}
}
