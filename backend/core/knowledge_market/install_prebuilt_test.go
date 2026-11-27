package knowledge_market

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"lazymind/core/asyncjob"
	"lazymind/core/common/orm"
	"lazymind/core/modelprovider"
	"lazymind/core/store"
)

// The algorithm boundary intentionally fails at KB creation. This checks
// Core's download and validation order without pretending that documents
// have already been imported.
func TestPrebuiltMarketInstallValidatesBeforeCreatingPersonalLibrary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		objectChange string
		wantCreate   bool
		wantError    string
	}{
		{name: "valid with no embedding model", wantCreate: true, wantError: "create dataset failed"},
		{name: "corrupt object", objectChange: "corrupt", wantError: "checksum"},
		{name: "interrupted object", objectChange: "interrupt", wantError: "download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const origin = "http://127.0.0.1:18117"
			const itemID = "local-prebuilt-demo"
			const userID = "demo-reader"
			files, objectPath := prebuiltDemoFiles(t)
			catalog := fmt.Sprintf(`
knowledge_market_items:
  - id: %s
    category: industry
    name: Local prebuilt demo
    package_url: %s/latest.json
    prebuilt_release_url: %s/latest.json
`, itemID, origin, origin)
			db := newUpdateTestDB(t)
			if err := SeedCatalog(context.Background(), db, writeCatalog(t, catalog)); err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&orm.EmbeddingIndexState{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&orm.EmbeddingIndexState{Scope: "global", Phase: "ready"}).Error; err != nil {
				t.Fatal(err)
			}
			store.Init(db, db, nil)
			t.Cleanup(func() { store.Init(nil, nil, nil) })
			ready, err := modelprovider.IsModelReady(context.Background(), db, userID, "embed_main")
			if err != nil || ready {
				t.Fatalf("test requires no embedding model, got ready=%v, err=%v", ready, err)
			}
			t.Setenv("LAZYMIND_UPLOAD_ROOT", t.TempDir())
			t.Setenv("LAZYMIND_ALGO_SERVICE_URL", "http://127.0.0.1:18118")

			got := make(map[string]bool)
			creates := 0
			transport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == http.MethodGet && r.URL.Host == "127.0.0.1:18117":
					data, ok := files[r.URL.Path]
					if !ok {
						t.Errorf("unexpected package path %s", r.URL.Path)
						return testJSONResponse(http.StatusNotFound, "{}"), nil
					}
					got[r.URL.Path] = true
					var body io.Reader = bytes.NewReader(data)
					if r.URL.Path == objectPath {
						switch tc.objectChange {
						case "corrupt":
							data = bytes.Clone(data)
							// Change the GZIP timestamp without damaging its payload or CRC.
							data[4] ^= 1
							reader, err := gzip.NewReader(bytes.NewReader(data))
							if err != nil {
								t.Errorf("changed object must still open as gzip: %v", err)
								return nil, err
							}
							_, err = io.Copy(io.Discard, reader)
							reader.Close()
							if err != nil {
								t.Errorf("changed object must still decompress: %v", err)
								return nil, err
							}
							body = bytes.NewReader(data)
						case "interrupt":
							body = io.MultiReader(bytes.NewReader(data[:len(data)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
						}
					}
					return &http.Response{
						StatusCode:    http.StatusOK,
						Header:        make(http.Header),
						Body:          io.NopCloser(body),
						ContentLength: int64(len(data)),
					}, nil
				case r.Method == http.MethodPost && r.URL.Host == "127.0.0.1:18118" && r.URL.Path == "/v1/kbs":
					creates++
					for path := range files {
						if !got[path] {
							t.Errorf("KB creation started before downloading %s", path)
						}
					}
					return testJSONResponse(http.StatusBadGateway, `{"message":"test stops at KB creation"}`), nil
				default:
					t.Errorf("unexpected external request %s %s", r.Method, r.URL)
					return testJSONResponse(http.StatusNotFound, "{}"), nil
				}
			})
			t.Cleanup(func() { http.DefaultTransport = transport })

			payload, err := json.Marshal(installJobPayload{MarketItemID: itemID, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			_, err = HandleInstallJob(context.Background(), asyncjob.Job{ID: "prebuilt-demo-job", PayloadJSON: payload}, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantError) {
				t.Errorf("install error = %v, want %q", err, tc.wantError)
			}
			if !got["/latest.json"] || !got["/releases/v1/manifest.json"] || !got[objectPath] {
				t.Errorf("install must fetch the release, its manifest and the tested object, got %v", got)
			}
			wantCreates := 0
			if tc.wantCreate {
				wantCreates = 1
			}
			if creates != wantCreates {
				t.Errorf("KB create requests = %d, want %d", creates, wantCreates)
			}
			install, loadErr := loadMarketInstallRow(context.Background(), db, userID, itemID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if install != nil && install.DatasetID != "" {
				t.Errorf("failed installation linked to personal knowledge base %q", install.DatasetID)
			}
		})
	}
}

func prebuiltDemoFiles(t *testing.T) (map[string][]byte, string) {
	t.Helper()
	tree := os.DirFS("testdata/local_prebuilt_demo")
	files := make(map[string][]byte)
	var firstObject string
	err := fs.WalkDir(tree, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		files["/"+path] = data
		if firstObject == "" && strings.HasPrefix(path, "objects/") {
			firstObject = "/" + path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 || firstObject == "" || files["/latest.json"] == nil || files["/releases/v1/manifest.json"] == nil {
		t.Fatalf("expected a release, a manifest and two objects, got %d files", len(files))
	}
	return files, firstObject
}
