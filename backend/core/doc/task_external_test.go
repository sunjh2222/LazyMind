package doc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCallExternalAddDocsForwardsPrebuiltPathWithoutModelConfig(t *testing.T) {
	const endpoint = "http://doc-server.test"
	const prebuiltPath = "/uploads/demo/阅览室 切片.jsonl.gz"
	t.Setenv("LAZYMIND_DOCUMENT_SERVICE_URL", endpoint)
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != endpoint+"/v1/docs/add" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		var body struct {
			ProcessingLevel string          `json:"processing_level"`
			ModelConfig     json.RawMessage `json:"llm_config"`
			Items           []struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode add-docs request: %v", err)
		}
		if body.ProcessingLevel != "chunked" || len(body.ModelConfig) != 0 {
			t.Errorf("want chunked without llm_config, got level=%q config=%s", body.ProcessingLevel, body.ModelConfig)
		}
		if len(body.Items) != 1 || body.Items[0].Metadata["prebuilt_path"] != prebuiltPath {
			t.Errorf("want unchanged prebuilt_path in item metadata, got %+v", body.Items)
		}
		return testJSONResponse(http.StatusOK, `{"code":200,"data":{"items":[{"task_id":"demo-task"}]}}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })

	result, err := callExternalAddDocsContext(context.Background(), addRequest{
		Items:           []addFileItem{{FilePath: "/uploads/demo/reading-room.md", Metadata: map[string]any{"prebuilt_path": prebuiltPath}}},
		ProcessingLevel: ProcessingLevelChunked,
	})
	if err != nil || len(result) != 1 || result[0].TaskID != "demo-task" {
		t.Fatalf("add docs did not submit the request: result=%+v err=%v", result, err)
	}
}

func TestParseReparseTaskIDsAcceptsWrappedDocServerResponse(t *testing.T) {
	var resp reparseResponse
	resp.Data.TaskIDs = []string{"reparse-task-1"}

	got := parseReparseTaskIDs(resp)
	if len(got) != 1 || got[0] != "reparse-task-1" {
		t.Fatalf("expected wrapped reparse task id, got %#v", got)
	}
}

func TestParseReparseTaskIDsPrefersTopLevelTaskIDs(t *testing.T) {
	resp := reparseResponse{TaskIDs: []string{"top-level-task"}}
	resp.Data.TaskIDs = []string{"wrapped-task"}

	got := parseReparseTaskIDs(resp)
	if len(got) != 1 || got[0] != "top-level-task" {
		t.Fatalf("expected top-level reparse task id, got %#v", got)
	}
}
