package knowledge_market

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"

	"lazymind/core/common/orm"
	"lazymind/core/knowledge_market/download"
	"lazymind/core/store"
)

const coexistenceCatalog = `
knowledge_market_items:
  - id: local-prebuilt-demo
    category: industry
    name: Local prebuilt demo
    package_url: https://example.test/original.md
    source_adapter: generic_markdown
    prebuilt_release_url: http://127.0.0.1:18117/latest.json
`

func TestPrebuiltInstallPinsManifestBeforeAcceptingJob(t *testing.T) {
	db := newUpdateTestDB(t)
	if err := SeedCatalog(context.Background(), db, writeCatalog(t, coexistenceCatalog)); err != nil {
		t.Fatal(err)
	}
	store.Init(db, db, nil)
	t.Cleanup(func() { store.Init(nil, nil, nil) })
	files, _ := prebuiltDemoFiles(t)
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	calls := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "http://127.0.0.1:18117/latest.json" {
			t.Errorf("unexpected request during install acceptance: %s", r.URL)
			return testJSONResponse(http.StatusNotFound, ""), nil
		}
		return testJSONResponse(http.StatusOK, string(files["/latest.json"])), nil
	})
	router := mux.NewRouter()
	router.HandleFunc("/knowledge-market/items/{market_item_id}:install", MarketInstall)
	rec := performPost(t, router, "/knowledge-market/items/local-prebuilt-demo:install", "reader")
	if rec.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", rec.Code, rec.Body.String())
	}
	var queued orm.AsyncJob
	if err := db.First(&queued).Error; err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Prebuilt *prebuiltRelease `json:"prebuilt"`
	}
	if err := json.Unmarshal(queued.PayloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || payload.Prebuilt == nil || payload.Prebuilt.Version != "v1" ||
		!matchesPrebuiltHash(files["/releases/v1/manifest.json"], payload.Prebuilt.ManifestSHA256) {
		t.Fatalf("accepted without pinning the release: calls=%d payload=%s", calls, queued.PayloadJSON)
	}
}

func TestOptionalPrebuiltReleasePreservesOriginalUpdate(t *testing.T) {
	db := newUpdateTestDB(t)
	if err := SeedCatalog(context.Background(), db, writeCatalog(t, coexistenceCatalog)); err != nil {
		t.Fatal(err)
	}
	store.Init(db, db, nil)
	t.Cleanup(func() { store.Init(nil, nil, nil) })
	if err := db.Create(&orm.KnowledgeMarketInstall{
		MarketItemID: "local-prebuilt-demo", UserID: "reader", DatasetID: "old-library",
		InstallState: string(orm.InstallStateDone), Config: json.RawMessage(`{}`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	rec := performPost(t, updateRouter(), "/knowledge-market/items/local-prebuilt-demo:update", "reader")
	if rec.Code != http.StatusOK {
		t.Errorf("original updates must use their existing path: %d %s", rec.Code, rec.Body.String())
	}
	var item orm.KnowledgeMarketItem
	if err := db.First(&item).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.md"), []byte("# Source\nOriginal text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := materializeMarketFiles(context.Background(), item, []download.FetchedFile{{Path: "source.md"}}, root)
	if err != nil || len(files) != 1 || files[0].PrebuiltPath != "" {
		t.Errorf("original Markdown adapter could not select source file: %v %v", files, err)
	}
}

func TestPrebuiltWaitingForWorkerUsesImportProgress(t *testing.T) {
	install := &orm.KnowledgeMarketInstall{
		InstallState: string(orm.InstallStateDone),
		Config:       json.RawMessage(`{"prebuilt":{"version":"v1"}}`),
	}
	parse := parseProgressInfo{State: "parsing", Total: 2, Done: 1, Parsing: 1}
	job := orm.AsyncJob{
		Status: "succeeded", PayloadJSON: json.RawMessage(`{"prebuilt":{"version":"v1"}}`),
		ResultJSON: json.RawMessage(`{"dataset_id":"library","task_ids":["a","b"]}`),
	}
	own, _ := taskProgressSource(job, install, func() bool { return true })
	effective := installWithEffectiveState(own, parse)
	stage, percent := installStageAndPercent(job, effective, parse)
	if effective.InstallState != string(orm.InstallStateImporting) || stage != "importing" || percent != 80 {
		t.Errorf("prebuilt worker status = %s/%s/%d, want importing/importing/80", effective.InstallState, stage, percent)
	}
}
