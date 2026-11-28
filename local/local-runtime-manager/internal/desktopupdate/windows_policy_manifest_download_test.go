//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const windowsManifestDownloadPath = "/releases/r2/installed-manifest.json"

func newWindowsManifestDownloadFixture(t *testing.T) *transferFixture {
	t.Helper()
	f := newTransferFixtureForPolicy(t, newWindowsPolicyManifestFixture(t))
	for _, item := range f.policy["releases"].([]any) {
		release := item.(map[string]any)
		id := release["releaseId"].(string)
		path := "/releases/" + id + "/installed-manifest.json"
		body := []byte("synthetic installed manifest for " + id)
		f.files[path] = body
		digest := sha256.Sum256(body)
		release["installedManifest"] = Artifact{
			URL: f.server.URL + path, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(body)),
		}
	}
	return f
}

func windowsManifestReceiptPath(t *testing.T, receipt DownloadReceipt) string {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InstalledManifestPath string `json:"installedManifestPath"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.InstalledManifestPath
}

func TestWindowsPolicyManifestDownloadBindsSelectedArtifact(t *testing.T) {
	f := newWindowsManifestDownloadFixture(t)
	var final Progress
	receipt, err := Download(t.Context(), f.config, f.state, f.root, "r2", f.server.Client(), policyTime, func(p Progress) { final = p })
	if err != nil || receipt.Status != "downloaded" || receipt.ReleaseID != "r2" || receipt.PackagePath == "" || receipt.ComponentsPath == "" {
		t.Fatalf("unexpected download receipt: %+v, err=%v", receipt, err)
	}
	if _, requested := f.artifactRequests.Load(windowsManifestDownloadPath); !requested {
		t.Error("selected release's installed manifest was not requested")
	}
	for _, path := range []string{"/releases/r1/installed-manifest.json", "/releases/r3/installed-manifest.json", "/r2/feed"} {
		if _, requested := f.artifactRequests.Load(path); requested {
			t.Errorf("requested an artifact outside the selected v2 download: %s", path)
		}
	}
	if receipt.FeedPath != "" {
		t.Errorf("v2 receipt contains a remote feed: %q", receipt.FeedPath)
	}
	wantBytes := f.files[windowsManifestDownloadPath]
	path := windowsManifestReceiptPath(t, receipt)
	if path == "" {
		t.Error("download did not return installedManifestPath")
	} else {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, wantBytes) {
			t.Errorf("receipt path did not contain the exact pinned manifest: %q, err=%v", actual, err)
		}
		digest := sha256.Sum256(wantBytes)
		name, sha := filepath.Base(path), hex.EncodeToString(digest[:])
		relative, err := filepath.Rel(filepath.Join(f.root, "downloads"), path)
		if err != nil || !filepath.IsLocal(relative) || filepath.Dir(path) != filepath.Dir(receipt.PackagePath) ||
			(name != sha && !strings.HasPrefix(name, sha+".")) {
			t.Errorf("manifest path is outside the existing content-addressed cache: %q", path)
		}
	}
	wantTotal := int64(len(f.files["/r2/package"]) + len(f.files["/r2/components"]) + len(wantBytes))
	if final.TotalBytes != wantTotal || final.DownloadedBytes != wantTotal {
		t.Errorf("final download progress = %+v, want %d completed bytes", final, wantTotal)
	}
}

func TestWindowsPolicyManifestDownloadRejectsUnverifiedBytes(t *testing.T) {
	for _, tc := range []struct {
		name, wantError string
		change          func([]byte) []byte
	}{
		{"same length wrong digest", "artifact_hash", func(body []byte) []byte {
			changed := bytes.Clone(body)
			changed[0] ^= 1
			return changed
		}},
		{"wrong length", "artifact_size", func(body []byte) []byte { return body[:len(body)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsManifestDownloadFixture(t)
			f.serveArtifacts[windowsManifestDownloadPath] = func(w http.ResponseWriter, _ *http.Request, body []byte) {
				_, _ = w.Write(tc.change(body))
			}
			receipt, err := f.download(t)
			if ErrorCode(err) != tc.wantError || receipt.Status == "downloaded" || windowsManifestReceiptPath(t, receipt) != "" {
				t.Errorf("unverified manifest download: receipt=%+v err=%v, want %s with no receipt path", receipt, err, tc.wantError)
			}
			if _, requested := f.artifactRequests.Load(windowsManifestDownloadPath); !requested {
				t.Error("corrupt manifest response was never exercised")
			}
			digest := sha256.Sum256(f.files[windowsManifestDownloadPath])
			sha := hex.EncodeToString(digest[:])
			if err := filepath.WalkDir(filepath.Join(f.root, "downloads"), func(_ string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && (entry.Name() == sha || strings.HasPrefix(entry.Name(), sha+".")) {
					t.Errorf("unverified manifest was promoted to a content-addressed cache entry: %q", entry.Name())
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
