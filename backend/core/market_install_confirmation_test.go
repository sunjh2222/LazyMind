package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"lazymind/core/common/orm"
	"lazymind/core/knowledge_market"
	"lazymind/core/modelprovider"
	"lazymind/core/store"
)

const installConfirmItem = "local-prebuilt-demo"
const installConfirmPath = "/knowledge-market/items/" + installConfirmItem
const installConfirmReader = "confirm-reader"
const installConfirmModelName = "Fixture embedding"
const installConfirmOrigin = "http://127.0.0.1:18117"

type installConfirmTransport func(*http.Request) (*http.Response, error)

func (f installConfirmTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type installConfirmCatalog struct{}

func (installConfirmCatalog) CloudModelCatalog(context.Context) (modelprovider.CloudModelCatalog, error) {
	return modelprovider.CloudModelCatalog{Known: true, Available: true, HasTokenPlan: true,
		Models: []modelprovider.CloudCatalogModel{
			{ModelKey: "embed-a", DisplayName: installConfirmModelName, ModelType: "embed_main", Status: "available"},
			{ModelKey: "embed-b", DisplayName: installConfirmModelName, ModelType: "embed_main", Status: "available"},
		}}, nil
}

type installConfirmRelease struct {
	Version        string `json:"version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ManifestPath   string `json:"manifest_path"`
	ItemID         string `json:"item_id"`
	DocumentCount  int    `json:"document_count"`
	ChunkCount     int    `json:"chunk_count"`
	SizeBytes      int64  `json:"size_bytes"`
}

type installConfirmFixture struct {
	t        *testing.T
	db       *orm.DB
	router   *mux.Router
	latest   []byte
	unmoved  installConfirmRelease
	manifest []byte
	next     []byte
}

func newInstallConfirmFixture(t *testing.T, ready bool) *installConfirmFixture {
	t.Helper()
	return newInstallConfirmFixtureWithTrust(t, ready, strings.Repeat("f", 64))
}

func newInstallConfirmFixtureWithTrust(t *testing.T, ready bool, signingSecret string) *installConfirmFixture {
	t.Helper()
	t.Setenv("LAZYMIND_RAG_ENABLED", "true")
	t.Setenv("LAZYMIND_CLOUD_BASE_URL", "")
	t.Setenv("LAZYMIND_FILE_URL_SIGN_SECRET", signingSecret)
	t.Setenv("LAZYMIND_AUTH_SERVICE_INTERNAL_TOKEN", "fixture-internal-token")
	db := orm.MigrateTestDB(t, &orm.KnowledgeMarketItem{}, &orm.KnowledgeMarketInstall{}, &orm.AsyncJob{},
		&orm.Dataset{}, &orm.Document{}, &orm.Task{}, &orm.EmbeddingIndexState{}, &orm.DocumentProcessingState{})
	store.Init(db.DB, db.DB, nil)
	t.Cleanup(func() { store.Init(nil, nil, nil) })
	fixture := &installConfirmFixture{t: t, db: db, router: mux.NewRouter()}
	model := orm.EmbeddingIndexState{Scope: "global", Phase: "ready", Generation: 7}
	if ready {
		model.ActiveSource, model.ActiveModelID = "cloud", "embed-a"
		model.ActiveModelName, model.ActiveProviderName = installConfirmModelName, "Fixture catalog"
		model.Signature = "v1:4:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64)
	}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("knowledge_market", "testdata", "local_prebuilt_demo")
	var err error
	fixture.latest, err = os.ReadFile(filepath.Join(root, "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.latest, &fixture.unmoved); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(fixture.unmoved.ManifestPath)))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(manifest)) != fixture.unmoved.ManifestSHA256 {
		t.Fatalf("could not verify frozen manifest: %v", err)
	}
	fixture.manifest = manifest
	var movedManifest map[string]any
	if err := json.Unmarshal(manifest, &movedManifest); err != nil {
		t.Fatal(err)
	}
	movedManifest["version"] = "v2"
	nextManifest, err := json.Marshal(movedManifest)
	if err != nil {
		t.Fatal(err)
	}
	moved := fixture.unmoved
	moved.Version, moved.ManifestPath = "v2", "releases/v2/manifest.json"
	moved.ManifestSHA256 = fmt.Sprintf("%x", sha256.Sum256(nextManifest))
	fixture.next, err = json.Marshal(moved)
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.yaml")
	config := `knowledge_market_items:
  - id: local-prebuilt-demo
    category: industry
    name: Install confirmation fixture
    package_url: https://original.example.test/original.md
    prebuilt_release_url: http://127.0.0.1:18117/latest.json
`
	if err := os.WriteFile(catalog, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := knowledge_market.SeedCatalog(t.Context(), db.DB, catalog); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = installConfirmTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Scheme+"://"+r.URL.Host != installConfirmOrigin || r.URL.RawQuery != "" {
			t.Fatalf("unexpected outbound request: %s %s", r.Method, r.URL.EscapedPath())
		}
		var payload []byte
		switch r.URL.Path {
		case "/latest.json":
			payload = fixture.latest
		case "/" + fixture.unmoved.ManifestPath:
			payload = fixture.manifest
		case "/" + moved.ManifestPath:
			payload = nextManifest
		default:
			t.Fatalf("request downloaded an object or contacted an unapproved path: %s", r.URL.EscapedPath())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload))}, nil
	})
	registerAllRoutes(fixture.router)
	// Route registration sets the cloud runtime provider. Substitute the
	// offline catalog afterward so readiness uses the same fixture as requests.
	modelprovider.SetCloudCatalogProvider(installConfirmCatalog{})
	t.Cleanup(func() { modelprovider.SetCloudCatalogProvider(nil) })
	if got, err := modelprovider.IsModelReady(t.Context(), db.DB, installConfirmReader, "embed_main"); err != nil || got != ready {
		t.Fatalf("expected actual model readiness %t, got %t (%v)", ready, got, err)
	}
	return fixture
}

func (f *installConfirmFixture) request(method, path, user, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var content io.Reader
	if body != "" {
		content = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, installConfirmPath+path, content).WithContext(f.t.Context())
	if user != "" {
		r.Header.Set("X-User-Id", user)
	}
	if content != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func (f *installConfirmFixture) assertCounts(jobs int64) {
	f.t.Helper()
	for _, check := range []struct {
		model any
		count int64
	}{{&orm.AsyncJob{}, jobs}, {&orm.KnowledgeMarketInstall{}, 0}, {&orm.Dataset{}, 0}, {&orm.Document{}, 0}, {&orm.Task{}, 0}} {
		var count int64
		if err := f.db.Model(check.model).Count(&count).Error; err != nil || count != check.count {
			f.t.Fatalf("%T rows = %d, want %d (%v)", check.model, count, check.count, err)
		}
	}
}

func (f *installConfirmFixture) preview(ready bool) string {
	f.t.Helper()
	w := f.request(http.MethodGet, "/install-preview", installConfirmReader, "")
	var response struct {
		Code int `json:"code"`
		Data struct {
			installConfirmRelease
			Vector *struct {
				Available *bool  `json:"available"`
				ModelName string `json:"model_name"`
				Token     string `json:"confirmation_token"`
			} `json:"vector_index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); w.Code != http.StatusOK || err != nil || response.Code != 0 {
		f.t.Fatalf("install-preview status=%d body=%s (%v)", w.Code, w.Body.String(), err)
	}
	data, vector := response.Data, response.Data.Vector
	if data.Version != "v1" || data.ManifestSHA256 != f.unmoved.ManifestSHA256 || data.DocumentCount != 2 ||
		data.ChunkCount != 21 || data.SizeBytes != 1519 || vector == nil || vector.Available == nil || *vector.Available != ready {
		f.t.Fatalf("preview does not describe the fixed release and current model readiness: %s", w.Body.String())
	}
	if (ready && (vector.Token == "" || vector.ModelName != installConfirmModelName)) || (!ready && vector.Token != "") {
		f.t.Fatal("preview must only issue confirmation for the available model it displays")
	}
	for _, field := range []string{`"api_key"`, `"base_url"`, `"credentials"`, `"llm_config"`, `"model_config"`, "fixture-internal-token"} {
		if strings.Contains(w.Body.String(), field) {
			f.t.Fatalf("install preview exposed an internal model field: %q", field)
		}
	}
	f.assertCounts(0)
	return vector.Token
}

func (f *installConfirmFixture) assertQueued(w *httptest.ResponseRecorder, indexed, originalSource bool) {
	f.t.Helper()
	data := marketControlData(f.t, w)
	jobID, _ := data["job_id"].(string)
	var job orm.AsyncJob
	if jobID == "" {
		f.t.Fatal("install response did not identify its queued job")
	}
	if err := f.db.Take(&job, "id = ?", jobID).Error; err != nil {
		f.t.Fatal(err)
	}
	var payload struct {
		UserID       string `json:"user_id"`
		MarketItemID string `json:"market_item_id"`
		Prebuilt     *struct {
			installConfirmRelease
			URL string `json:"url"`
		} `json:"prebuilt"`
		ProcessingLevel   string `json:"processing_level"`
		OriginalSource    bool   `json:"original_source"`
		IndexConfirmation *struct {
			Source     string `json:"source"`
			ModelID    string `json:"model_id"`
			Signature  string `json:"signature"`
			Generation int64  `json:"generation"`
		} `json:"index_confirmation"`
	}
	if err := json.Unmarshal(job.PayloadJSON, &payload); err != nil ||
		job.ResourceID != installConfirmItem || job.CreateUserID != installConfirmReader || job.Status != "pending" || job.JobType != "knowledge_market_install" ||
		payload.UserID != installConfirmReader || payload.MarketItemID != installConfirmItem {
		f.t.Fatalf("expected a pending job for this install: %s (%v)", job.PayloadJSON, err)
	}
	if originalSource {
		if !payload.OriginalSource || payload.Prebuilt != nil || payload.IndexConfirmation != nil {
			f.t.Fatalf("old original-source install acquired a prebuilt confirmation: %s", job.PayloadJSON)
		}
		f.assertCounts(1)
		return
	}
	if payload.OriginalSource || payload.Prebuilt == nil || payload.Prebuilt.installConfirmRelease != f.unmoved ||
		payload.Prebuilt.URL != installConfirmOrigin+"/latest.json" {
		f.t.Fatalf("expected a job for the exact confirmed prebuilt release: %s", job.PayloadJSON)
	}
	if (indexed && payload.ProcessingLevel != "indexed") || (!indexed && payload.ProcessingLevel != "" && payload.ProcessingLevel != "chunked") {
		f.t.Fatalf("queued processing level = %q, indexed requested=%t", payload.ProcessingLevel, indexed)
	}
	if indexed {
		var model orm.EmbeddingIndexState
		if err := f.db.Take(&model, "scope = ?", "global").Error; err != nil {
			f.t.Fatal(err)
		}
		confirmation := payload.IndexConfirmation
		if confirmation == nil || confirmation.Source != "cloud" || confirmation.ModelID != "embed-a" ||
			confirmation.Signature != model.Signature || confirmation.Generation != 7 {
			f.t.Fatalf("queued indexed install lost its exact confirmed model identity: %+v", confirmation)
		}
	} else if payload.IndexConfirmation != nil {
		f.t.Fatal("text-only install must not carry a vector confirmation")
	}
	f.assertCounts(1)
}

func TestMarketInstallConfirmationRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body, user string
		ready, original  bool
		want             int
	}{
		{name: "legacy empty body defaults to text", user: installConfirmReader, want: http.StatusOK},
		{name: "legacy original-source install with empty body", user: installConfirmReader, original: true, want: http.StatusOK},
		{name: "explicit text without an embedding model", user: installConfirmReader, body: `{"processing_level":"chunked"}`, want: http.StatusOK},
		{name: "unknown processing level", user: installConfirmReader, body: `{"processing_level":"invalid"}`, want: http.StatusBadRequest},
		{name: "indexed without confirmation", user: installConfirmReader, body: `{"processing_level":"indexed"}`, ready: true, want: http.StatusBadRequest},
		{name: "missing user", body: `{"processing_level":"chunked"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallConfirmFixture(t, tc.ready)
			if tc.original {
				if err := f.db.Model(&orm.KnowledgeMarketItem{}).Where("id = ?", installConfirmItem).
					Update("source_options", json.RawMessage(`{}`)).Error; err != nil {
					t.Fatal(err)
				}
			}
			w := f.request(http.MethodPost, ":install", tc.user, tc.body)
			if tc.want == http.StatusOK {
				f.assertQueued(w, false, tc.original)
				return
			}
			if (tc.want != 0 && w.Code != tc.want) || (tc.want == 0 && w.Code != http.StatusBadRequest && w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden) {
				t.Errorf("install rejected with unexpected status %d (explicit want %d): %s", w.Code, tc.want, w.Body.String())
			}
			f.assertCounts(0)
		})
	}
}

func TestMarketInstallConfirmationPreviewWithoutModel(t *testing.T) {
	newInstallConfirmFixture(t, false).preview(false)
}

func TestMarketInstallConfirmationToken(t *testing.T) {
	for _, scenario := range []string{
		"current", "tampered confirmation", "release changed", "same version with a new manifest digest",
		"same-name model changed", "generation changed", "signature changed", "model unavailable", "different user",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newInstallConfirmFixture(t, true)
			token := f.preview(true)
			actor := installConfirmReader
			switch scenario {
			case "tampered confirmation":
				token += "X"
			case "release changed":
				f.latest = f.next
			case "same version with a new manifest digest":
				// The new bytes are still valid JSON with the same release metadata.
				f.manifest = append(bytes.Clone(f.manifest), '\n')
				moved := f.unmoved
				moved.ManifestSHA256 = fmt.Sprintf("%x", sha256.Sum256(f.manifest))
				var err error
				f.latest, err = json.Marshal(moved)
				if err != nil {
					t.Fatal(err)
				}
			case "same-name model changed", "generation changed", "signature changed":
				field, value := "active_model_id", any("embed-b")
				if scenario == "generation changed" {
					field, value = "generation", int64(8)
				} else if scenario == "signature changed" {
					field, value = "signature", "v1:4:"+strings.Repeat("c", 64)+":"+strings.Repeat("b", 64)
				}
				if err := f.db.Model(&orm.EmbeddingIndexState{}).Where("scope = ?", "global").UpdateColumn(field, value).Error; err != nil {
					t.Fatal(err)
				}
				if ready, err := modelprovider.IsModelReady(t.Context(), f.db.DB, actor, "embed_main"); err != nil || !ready {
					t.Fatalf("the model must remain ready after changing %s: %v", field, err)
				}
			case "model unavailable":
				if err := f.db.Model(&orm.EmbeddingIndexState{}).Where("scope = ?", "global").Update("phase", "clearing").Error; err != nil {
					t.Fatal(err)
				}
				if ready, err := modelprovider.IsModelReady(t.Context(), f.db.DB, actor, "embed_main"); err != nil || ready {
					t.Fatalf("model must no longer be ready: %v", err)
				}
			case "different user":
				actor = "another-reader"
			}
			body, err := json.Marshal(map[string]string{"processing_level": "indexed", "confirmation_token": token})
			if err != nil {
				t.Fatal(err)
			}
			w := f.request(http.MethodPost, ":install", actor, string(body))
			if scenario == "current" {
				f.assertQueued(w, true, false)
				return
			}
			if w.Code != http.StatusConflict && !(scenario == "different user" && w.Code == http.StatusForbidden) &&
				!(scenario == "tampered confirmation" && w.Code == http.StatusBadRequest) {
				t.Errorf("invalid confirmation accepted or returned the wrong status: %d %s", w.Code, w.Body.String())
			}
			f.assertCounts(0)
		})
	}
}

func TestMarketInstallConfirmationRejectsForeignInstance(t *testing.T) {
	const childTokenFile = "LAZYMIND_TEST_MARKET_CONFIRMATION_CHILD_TOKEN_FILE"
	t.Setenv("TEST_DB_DRIVER", "sqlite")
	post := func(f *installConfirmFixture, token string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"processing_level": "indexed", "confirmation_token": token})
		if err != nil {
			t.Fatal(err)
		}
		return f.request(http.MethodPost, ":install", installConfirmReader, string(body))
	}
	if path := os.Getenv(childTokenFile); path != "" {
		issuer := newInstallConfirmFixtureWithTrust(t, true, strings.Repeat("a", 64))
		token := issuer.preview(true)
		issuer.assertQueued(post(issuer, token), true, false)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(token)
		if closeErr := file.Close(); writeErr != nil || closeErr != nil {
			t.Fatalf("share the fixture confirmation securely: write=%v close=%v", writeErr, closeErr)
		}
		return
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "confirmation")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestMarketInstallConfirmationRejectsForeignInstance$")
	cmd.Env = append(os.Environ(), childTokenFile+"="+file)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated issuer did not issue and accept its own confirmation: %v\n%s", err, output)
	}
	token, err := os.ReadFile(file)
	if err != nil || len(token) == 0 {
		t.Fatalf("missing confirmation from isolated issuer: %v", err)
	}
	receiver := newInstallConfirmFixtureWithTrust(t, true, strings.Repeat("b", 64))
	receiver.preview(true)
	w := post(receiver, string(token))
	if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict && w.Code != http.StatusForbidden {
		t.Errorf("install with a confirmation issued by another instance returned HTTP %d", w.Code)
	}
	receiver.assertCounts(0)
}
