package knowledge_market

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"gorm.io/gorm"

	"lazymind/core/common/orm"
	"lazymind/core/common/readonlyorm"
	"lazymind/core/store"
)

const (
	previewItemID    = "local-prebuilt-demo"
	previewUserID    = "reader"
	previewDatasetID = "existing-library"
	previewHost      = "packages.example.test"
	previewOrigin    = "https://" + previewHost
	previewRoute     = "/knowledge-market/items/" + previewItemID + "/update-preview"
)

type testPrebuiltPreview struct {
	InstalledVersion string   `json:"installed_version"`
	TargetVersion    string   `json:"target_version"`
	HasUpdate        bool     `json:"has_update"`
	Added            []string `json:"added"`
	Changed          []string `json:"changed"`
	Removed          []string `json:"removed"`
	UnchangedCount   int      `json:"unchanged_count"`
	DownloadBytes    int64    `json:"download_bytes"`
}

func previewJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func previewHash(label string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(label)))
}

func previewDocument(id, revision string, size int64) prebuiltDocument {
	digest := previewHash("object:" + id + ":" + revision)
	return prebuiltDocument{
		ID: id, Title: id, SourcePath: id + ".md",
		SourceSHA256:     previewHash("source:" + id + ":" + revision),
		ChunkFingerprint: previewHash("chunks:" + id + ":" + revision),
		ObjectPath:       "objects/" + digest + ".jsonl.gz", ObjectSHA256: digest,
		ChunkCount: 2, Size: size,
	}
}

func previewRelease(t *testing.T, version string, documents []prebuiltDocument) (prebuiltRelease, json.RawMessage) {
	t.Helper()
	body := previewJSON(t, prebuiltManifest{
		Schema: 1, ItemID: previewItemID, Version: version,
		SourceRevision: "source-" + version, ChunkProfile: "general_algo", Documents: documents,
	})
	pointer := prebuiltRelease{
		ItemID: previewItemID, Version: version, ManifestPath: "releases/" + version + "/manifest.json",
		ManifestSHA256: previewHash(string(body)), DocumentCount: len(documents),
	}
	for _, document := range documents {
		pointer.ChunkCount += document.ChunkCount
		pointer.Size += document.Size
	}
	return pointer, body
}

func previewMarket(t *testing.T, installed prebuiltRelease) (*gorm.DB, *mux.Router) {
	t.Helper()
	t.Setenv("LAZYMIND_READONLY_SCHEMA", "")
	db := newUpdateTestDB(t)
	if err := db.AutoMigrate(&orm.Task{}, &readonlyorm.LazyLLMDocServiceTaskRow{}); err != nil {
		t.Fatal(err)
	}
	catalog := fmt.Sprintf(`
knowledge_market_items:
  - id: %s
    category: industry
    name: Preview demo
    prebuilt_release_url: %s/latest.json
`, previewItemID, previewOrigin)
	if err := SeedCatalog(context.Background(), db, writeCatalog(t, catalog)); err != nil {
		t.Fatal(err)
	}
	store.Init(db, db, nil)
	t.Cleanup(func() { store.Init(nil, nil, nil) })
	now := time.Now().UTC()
	dataset := orm.Dataset{
		ID: previewDatasetID, KbID: previewDatasetID, DisplayName: "Existing library",
		ProcessingLevel: "chunked", Ext: json.RawMessage(`{"prebuilt_market":true}`),
		BaseModel: orm.BaseModel{CreateUserID: previewUserID, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&dataset).Error; err != nil {
		t.Fatal(err)
	}
	installed.URL = previewOrigin + "/latest.json"
	taskIDs := make([]string, 0, installed.DocumentCount)
	for i := 0; i < installed.DocumentCount; i++ {
		taskID := fmt.Sprintf("preview-task-%d", i)
		workerID := fmt.Sprintf("preview-worker-%d", i)
		documentID := fmt.Sprintf("local-%c", 'a'+i)
		insertLinkedTask(t, db, taskID, documentID, workerID, previewDatasetID)
		insertDocServiceTask(t, db, workerID, documentID, previewDatasetID, "SUCCESS")
		taskIDs = append(taskIDs, taskID)
	}
	install := orm.KnowledgeMarketInstall{
		MarketItemID: previewItemID, UserID: previewUserID, DatasetID: previewDatasetID,
		InstalledVersion: installed.Version, InstallState: string(orm.InstallStateDone), InstalledAt: &now,
		Config:    previewJSON(t, installConfig{Prebuilt: &installed, TaskIDs: taskIDs}),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&install).Error; err != nil {
		t.Fatal(err)
	}
	router := marketRouter()
	router.HandleFunc("/knowledge-market/items/{market_item_id}/update-preview", MarketPrebuiltUpdatePreview).Methods(http.MethodGet)
	return db, router
}

func interceptPreviewRequests(t *testing.T, files map[string]json.RawMessage) func(...string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		body, ok := files[request.URL.Path]
		if !ok || request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != previewHost {
			t.Errorf("unexpected preview request %s %s", request.Method, request.URL)
			return testJSONResponse(http.StatusNotFound, "{}"), nil
		}
		return testJSONResponse(http.StatusOK, string(body)), nil
	})
	return func(want ...string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		slices.Sort(paths)
		slices.Sort(want)
		if !slices.Equal(paths, want) {
			t.Errorf("preview GET paths = %v, want %v", paths, want)
		}
	}
}

func TestMarketPrebuiltUpdatePreviewDifferencesAreReadOnly(t *testing.T) {
	oldDocuments := []prebuiltDocument{
		previewDocument("a", "v1", 90), previewDocument("b", "v1", 100),
		previewDocument("c", "v1", 110), previewDocument("d", "v1", 120),
	}
	oldRelease, oldManifest := previewRelease(t, "v1", oldDocuments)
	newDocuments := slices.Clone(oldDocuments)
	newDocuments[1] = previewDocument("b", "v2", 185)
	newRelease, newManifest := previewRelease(t, "v2", newDocuments)
	replacedDocuments := []prebuiltDocument{oldDocuments[0], oldDocuments[2], oldDocuments[3], previewDocument("e", "v3", 165)}
	replacedRelease, replacedManifest := previewRelease(t, "v3", replacedDocuments)
	for _, scenario := range []struct {
		name    string
		pointer prebuiltRelease
		body    json.RawMessage
		added   []string
		changed []string
		removed []string
		bytes   int64
	}{
		{"same pointer", oldRelease, oldManifest, []string{}, []string{}, []string{}, 0},
		{"one changed document", newRelease, newManifest, []string{}, []string{"b"}, []string{}, 185},
		{"replace one document", replacedRelease, replacedManifest, []string{"e"}, []string{}, []string{"b"}, 165},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, router := previewMarket(t, oldRelease)
			for _, entry := range oldDocuments {
				doc := orm.Document{
					ID: "local-" + entry.ID, DatasetID: previewDatasetID, DisplayName: entry.Title,
					LazyllmDocID: "current-" + entry.ID, Ext: json.RawMessage(`{}`),
					BaseModel: orm.BaseModel{CreateUserID: previewUserID},
				}
				if err := db.Create(&doc).Error; err != nil {
					t.Fatal(err)
				}
			}
			state := func() json.RawMessage {
				t.Helper()
				var rows struct {
					Installs  []orm.KnowledgeMarketInstall
					Datasets  []orm.Dataset
					Documents []orm.Document
					Jobs      []orm.AsyncJob
				}
				for _, target := range []any{&rows.Installs, &rows.Datasets, &rows.Documents, &rows.Jobs} {
					if err := db.Find(target).Error; err != nil {
						t.Fatal(err)
					}
				}
				return previewJSON(t, rows)
			}
			before := state()
			files := map[string]json.RawMessage{"/latest.json": previewJSON(t, scenario.pointer)}
			wantGETs := []string{"/latest.json"}
			hasUpdate := scenario.pointer.Version != oldRelease.Version
			if hasUpdate {
				files["/"+oldRelease.ManifestPath] = oldManifest
				files["/"+scenario.pointer.ManifestPath] = scenario.body
				wantGETs = append(wantGETs, "/"+oldRelease.ManifestPath, "/"+scenario.pointer.ManifestPath)
			}
			gotGETs := interceptPreviewRequests(t, files)
			response := performGetWithUser(t, router, previewRoute, previewUserID)
			var result struct {
				Code int                 `json:"code"`
				Data testPrebuiltPreview `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.Code != 0 {
				t.Fatalf("preview HTTP %d: %s, decode error %v", response.Code, response.Body.String(), err)
			}
			want := testPrebuiltPreview{
				InstalledVersion: "v1", TargetVersion: scenario.pointer.Version, HasUpdate: hasUpdate,
				Added: scenario.added, Changed: scenario.changed, Removed: scenario.removed,
				UnchangedCount: len(oldDocuments) - len(scenario.changed) - len(scenario.removed), DownloadBytes: scenario.bytes,
			}
			if !reflect.DeepEqual(result.Data, want) {
				t.Errorf("preview = %+v, want %+v", result.Data, want)
			}
			gotGETs(wantGETs...)
			if after := state(); string(before) != string(after) {
				t.Fatal("preview changed installation, documents, libraries or jobs")
			}
		})
	}
}

func TestMarketPrebuiltUpdatePreviewRejectsInvalidInstallBeforeFetching(t *testing.T) {
	pointer, _ := previewRelease(t, "v1", []prebuiltDocument{previewDocument("a", "v1", 90)})
	for _, scenario := range []struct {
		name          string
		user          string
		install       map[string]any
		dataset       map[string]any
		newCatalogURL string
		status        int
	}{
		{name: "other user", user: "other-reader", status: http.StatusNotFound},
		{name: "original install", install: map[string]any{"config": json.RawMessage(`{}`)}, status: http.StatusConflict},
		{name: "incomplete install", install: map[string]any{"install_state": "importing"}, status: http.StatusConflict},
		{name: "missing library link", install: map[string]any{"dataset_id": ""}, status: http.StatusConflict},
		{name: "missing linked library", install: map[string]any{"dataset_id": "missing-library"}, status: http.StatusForbidden},
		{name: "inconsistent installed version", install: map[string]any{"installed_version": "v0"}, status: http.StatusConflict},
		{name: "no access to library", dataset: map[string]any{"create_user_id": "another-owner"}, status: http.StatusForbidden},
		{name: "catalog origin changed", newCatalogURL: "https://changed.example.test/latest.json", status: http.StatusConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, router := previewMarket(t, pointer)
			if scenario.install != nil {
				if err := db.Model(&orm.KnowledgeMarketInstall{}).
					Where("market_item_id = ? AND user_id = ?", previewItemID, previewUserID).Updates(scenario.install).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario.dataset != nil {
				if err := db.Model(&orm.Dataset{}).Where("id = ?", previewDatasetID).Updates(scenario.dataset).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario.newCatalogURL != "" {
				options := previewJSON(t, map[string]string{prebuiltReleaseOption: scenario.newCatalogURL})
				if err := db.Model(&orm.KnowledgeMarketItem{}).Where("id = ?", previewItemID).
					Update("source_options", options).Error; err != nil {
					t.Fatal(err)
				}
			}
			gotGETs := interceptPreviewRequests(t, nil)
			user := scenario.user
			if user == "" {
				user = previewUserID
			}
			response := performGetWithUser(t, router, previewRoute, user)
			if response.Code != scenario.status {
				t.Errorf("preview HTTP %d, want %d: %s", response.Code, scenario.status, response.Body.String())
			}
			gotGETs()
		})
	}
}

func TestMarketPrebuiltUpdatePreviewRejectsManifestReplacementAtSameVersion(t *testing.T) {
	installed, _ := previewRelease(t, "v1", []prebuiltDocument{previewDocument("a", "v1", 90)})
	_, router := previewMarket(t, installed)
	replaced, _ := previewRelease(t, "v1", []prebuiltDocument{previewDocument("a", "modified", 100)})
	gotGETs := interceptPreviewRequests(t, map[string]json.RawMessage{"/latest.json": previewJSON(t, replaced)})
	response := performGetWithUser(t, router, previewRoute, previewUserID)
	if response.Code != http.StatusBadGateway {
		t.Errorf("preview HTTP %d, want 502 for same-version replacement: %s", response.Code, response.Body.String())
	}
	gotGETs("/latest.json")
}

func TestMarketPrebuiltUpdatePreviewRejectsIncompleteWorkerFiles(t *testing.T) {
	pointer, _ := previewRelease(t, "v1", []prebuiltDocument{
		previewDocument("a", "v1", 90), previewDocument("b", "v1", 100),
	})
	for _, scenario := range []struct {
		status string
		state  string
	}{
		{status: "PARSING", state: "parsing"},
		{status: "FAILED", state: "partial_failed"},
	} {
		t.Run(scenario.status, func(t *testing.T) {
			db, router := previewMarket(t, pointer)
			updated := db.Model(&readonlyorm.LazyLLMDocServiceTaskRow{}).
				Where("task_id = ?", "preview-worker-0").Update("status", scenario.status)
			if updated.Error != nil || updated.RowsAffected != 1 {
				t.Fatalf("seed authoritative worker state: rows=%d err=%v", updated.RowsAffected, updated.Error)
			}
			var installed orm.KnowledgeMarketInstall
			if err := db.Take(&installed, "market_item_id = ? AND user_id = ?", previewItemID, previewUserID).Error; err != nil {
				t.Fatal(err)
			}
			request := &http.Request{}
			actual := parseProgress(request, db, &installed)
			if installed.InstallState != string(orm.InstallStateDone) || actual.State != scenario.state || actual.Done != 1 {
				t.Fatalf("expected persisted done with one successful file and live %s, got install=%s progress=%+v",
					scenario.state, installed.InstallState, actual)
			}
			gotGETs := interceptPreviewRequests(t, map[string]json.RawMessage{"/latest.json": previewJSON(t, pointer)})
			response := performGetWithUser(t, router, previewRoute, previewUserID)
			if response.Code != http.StatusConflict {
				t.Errorf("preview HTTP %d, want 409 for incomplete worker files: %s", response.Code, response.Body.String())
			}
			gotGETs()
		})
	}
}
