package systemdeps

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// Core receives the catalog digest from the already accepted desktop update.
// The fixture supplies that handoff and uses the real deferred selection,
// background worker, ZIP verification, and target interpreter from there.
func TestWindowsPolicyComponentRecoveryReplacesCorruptSelectedRevision(t *testing.T) {
	t.Run("invalid ready marker", func(t *testing.T) { checkWindowsPolicyComponentRecovery(t, false) })
	t.Run("verified revision with broken module", func(t *testing.T) { checkWindowsPolicyComponentRecovery(t, true) })
}

func checkWindowsPolicyComponentRecovery(t *testing.T, breakVerifiedModule bool) {
	t.Helper()
	_, targetPython := replacementInterpreter(t)
	f := newMaintenanceFixture(t, false)
	ids := []string{"cluster", "rag"}
	sources := map[string]PythonComponent{"rag": f.source}
	cluster, _ := replacementPayload(t, "cluster", "A", targetPython)
	sources["cluster"] = cluster
	writeFixtureComponent(t, f.runtimeRoot, cluster)
	if err := savePythonDescriptor(f.runtimeRoot, cluster); err != nil {
		t.Fatal(err)
	}
	targets := map[string]PythonComponent{}
	updates := map[string]map[string]PythonComponent{}
	archives := map[string][]byte{}
	damagedArchives := map[string][]byte{}
	for _, id := range ids {
		target, archive := replacementPayload(t, id, "B", targetPython)
		targets[id], archives[target.URL] = target, archive
		// Change the first local header's modification time. The ZIP stays
		// readable and the response size stays valid, but the approved hash differs.
		damaged := bytes.Clone(archive)
		damaged[10] ^= 1
		if _, err := zip.NewReader(bytes.NewReader(damaged), int64(len(damaged))); err != nil {
			t.Fatalf("damaged transport fixture is not a valid ZIP: %v", err)
		}
		damagedArchives[target.URL] = damaged
		updates[id] = map[string]PythonComponent{"source": sources[id], "target": target}
		if !breakVerifiedModule {
			// An interrupted attempt left B's exact revision directory unusable.
			writeFixtureComponent(t, f.runtimeRoot, target)
			writeMaintenanceFile(t, filepath.Join(pythonComponentDir(f.runtimeRoot, target), ".ready"), []byte("corrupt-ready"))
		}
	}
	catalog := fixtureJSON(t, map[string]any{"schemaVersion": 1, "platform": runtime.GOOS, "arch": runtime.GOARCH,
		"runtimeBaseFingerprint": targetPython.Base, "components": targets})
	hash := sha256.Sum256(catalog)
	acceptedCatalog := hex.EncodeToString(hash[:])
	f.record["componentsSHA256"], f.record["componentsSize"] = acceptedCatalog, len(catalog)
	f.record["componentUpdates"], f.record["wantedComponents"] = updates, ids
	delete(f.record, "sourceComponent")
	delete(f.record, "targetComponent")
	f.record["phase"] = "installing"
	f.record["installationRoot"] = f.targetBundle
	f.record["scope"].(map[string]any)["installation"] = f.targetBundle
	f.save(t)
	writeMaintenanceFile(t, f.record["componentsPath"].(string), catalog)
	writeMaintenanceFile(t, filepath.Join(f.targetBundle, "config", "python-components.json"), catalog)
	corePath := f.record["sourceStorage"].(map[string]any)["databases"].(map[string]string)["core"]
	oldData := recordWindowsComponentRecoveryKnowledge(t, corePath)
	checkRetained := func() {
		t.Helper()
		if raw, err := os.ReadFile(corePath); err != nil || !bytes.Equal(oldData, raw) {
			t.Fatalf("component recovery changed the original SQLite data: %v", err)
		}
		for _, old := range sources {
			if err := readMaintenanceSelection(f.runtimeRoot, old); err != nil {
				t.Fatalf("component recovery lost the source %s payload or receipt: %v", old.ID, err)
			}
		}
		selected, err := readSelectedComponentUpdate(f.runtimeRoot)
		if err != nil || !slices.Equal(selected.Wanted, ids) || selected.CatalogSHA256 != acceptedCatalog ||
			selected.TransactionID != f.record["transactionId"].(string) || selected.ReleaseID != f.record["targetReleaseId"].(string) {
			t.Fatalf("component recovery lost the accepted selection: %+v, %v", selected, err)
		}
		for id, old := range sources {
			if selected.SourceRevisions[id] != old.Revision {
				t.Fatalf("component recovery lost the source %s revision: %+v", id, selected.SourceRevisions)
			}
		}
	}
	f.client.Transport = maintenanceHTTP(func(*http.Request) (*http.Response, error) {
		t.Error("deferred selection must not download during application activation")
		return nil, io.ErrUnexpectedEOF
	})
	f.run(t, "defer", "deferred", "", true)
	if err := os.Remove(f.intent); err != nil {
		t.Fatal(err)
	}
	checkRetained()
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	start := func(downloads []string, bad bool, requests chan<- string, deadline time.Duration) (context.CancelFunc, <-chan error) {
		env := f.environment(true)
		env.Python = targetPython.Exe
		env.Client = &http.Client{Transport: maintenanceHTTP(func(req *http.Request) (*http.Response, error) {
			payload, ok := archives[req.URL.String()]
			if !ok || !slices.ContainsFunc(downloads, func(id string) bool { return targets[id].URL == req.URL.String() }) {
				t.Errorf("worker requested an asset outside the accepted catalog: %s", req.URL)
				return nil, io.ErrUnexpectedEOF
			}
			if bad {
				payload = damagedArchives[req.URL.String()]
				select {
				case requests <- req.URL.String():
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}
			return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(payload)), Header: make(http.Header),
				Request: req, Body: io.NopCloser(bytes.NewReader(payload))}, nil
		})}
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		done := make(chan error, 1)
		go func() { done <- env.ReconcileSelectedPythonComponents(ctx, 20*time.Millisecond) }()
		return cancel, done
	}
	checkVerified := func() {
		t.Helper()
		selected, err := readSelectedComponentUpdate(f.runtimeRoot)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if err := readMaintenanceSelection(f.runtimeRoot, target); err != nil || selected.Verified[target.ID] != target.Revision {
				t.Fatalf("verified %s revision was not durably installed: %v", target.ID, err)
			}
		}
	}
	downloads := ids
	var originalModule []byte
	var modulePath string
	if breakVerifiedModule {
		cancel, done := start(ids, false, nil, 4*time.Second)
		defer cancel()
		if err := <-done; err != nil {
			t.Fatalf("could not establish the originally verified Python modules: %v", err)
		}
		checkVerified()
		if pending, err := HasPendingSelectedComponentWork(f.runtimeRoot, f.targetBundle); err != nil || pending {
			t.Fatalf("healthy verified components unexpectedly require replacement: pending=%v err=%v", pending, err)
		}
		rag := targets["rag"]
		dir := pythonComponentDir(f.runtimeRoot, rag)
		modulePath = filepath.Join(dir, "site-packages", "pymilvus", "__init__.py")
		originalModule = read(modulePath)
		evidence := map[string][]byte{}
		for _, path := range []string{filepath.Join(dir, ".ready"), filepath.Join(dir, "bundle-manifest.json"), pythonDescriptorPath(f.runtimeRoot, rag), selectionFile(f.runtimeRoot)} {
			evidence[path] = read(path)
		}
		writeMaintenanceFile(t, modulePath, []byte("def broken_python_module(\n"))
		for path, expected := range evidence {
			if !bytes.Equal(expected, read(path)) {
				t.Fatalf("the fixture changed valid verification metadata: %s", path)
			}
		}
		downloads = []string{"rag"}
	}
	if pending, err := HasPendingSelectedComponentWork(f.runtimeRoot, f.targetBundle); err != nil || !pending {
		t.Errorf("startup preflight did not schedule the damaged selected revision: pending=%v err=%v", pending, err)
	}
	selectedBefore := read(selectionFile(f.runtimeRoot))
	// Require a second request per component so the worker has rejected the
	// first response, rather than merely being cancelled during its download.
	requests := make(chan string)
	cancel, done := start(downloads, true, requests, 2*time.Second)
	defer cancel()
	attempts := map[string]int{}
	finished := false
	for !finished && slices.ContainsFunc(downloads, func(id string) bool { return attempts[targets[id].URL] < 2 }) {
		select {
		case url := <-requests:
			attempts[url]++
		case err := <-done:
			t.Errorf("corrupt local revisions prevented component retries: attempts=%v err=%v", attempts, err)
			finished = true
		}
	}
	cancel()
	if !finished {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("damaged remote bytes ended recovery instead of leaving a retry: %v", err)
		}
	}
	checkRetained()
	if breakVerifiedModule {
		if pending, err := HasPendingSelectedComponentWork(f.runtimeRoot, f.targetBundle); err != nil || !pending {
			t.Errorf("startup preflight lost the retry after a damaged response: pending=%v err=%v", pending, err)
		}
		if raw, err := os.ReadFile(modulePath); err == nil && !bytes.Equal(raw, []byte("def broken_python_module(\n")) {
			t.Fatal("unverified transport changed the active module contents")
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := readMaintenanceSelection(f.runtimeRoot, targets["cluster"]); err != nil {
			t.Fatalf("RAG repair changed the healthy Cluster selection: %v", err)
		}
	} else {
		if !bytes.Equal(selectedBefore, read(selectionFile(f.runtimeRoot))) {
			t.Fatal("damaged remote bytes changed the durable selection")
		}
		for _, target := range targets {
			if pythonComponentInstalled(f.runtimeRoot, target) || readMaintenanceSelection(f.runtimeRoot, target) == nil {
				t.Fatalf("damaged %s bytes became ready or selected", target.ID)
			}
		}
	}

	// Reopen the same selection after the source for the pinned ZIP recovers.
	cancel, done = start(downloads, false, nil, 4*time.Second)
	defer cancel()
	if err := <-done; err != nil {
		t.Fatalf("valid pinned archives did not restore both selected revisions: %v", err)
	}
	checkRetained()
	checkVerified()
	if breakVerifiedModule && !bytes.Equal(originalModule, read(modulePath)) {
		t.Fatal("valid pinned ZIP did not restore the damaged module bytes")
	}
	for _, target := range targets {
		checkReplacementImport(t, targetPython, filepath.Join(pythonComponentDir(f.runtimeRoot, target), "site-packages"), target.ID, "B", true)
	}
}

func recordWindowsComponentRecoveryKnowledge(t *testing.T, filename string) []byte {
	t.Helper()
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE fixture_knowledge (id TEXT PRIMARY KEY, contents TEXT NOT NULL);
		INSERT INTO fixture_knowledge VALUES ('old-kb', 'Existing knowledge base contents')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var contents string
	if err := db.QueryRow(`SELECT contents FROM fixture_knowledge WHERE id = 'old-kb'`).Scan(&contents); err != nil || contents != "Existing knowledge base contents" {
		_ = db.Close()
		t.Fatalf("create old knowledge base fixture: %q, %v", contents, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
