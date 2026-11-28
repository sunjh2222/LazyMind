package desktopupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type installedManifestFixture struct {
	policy   *policyFixture
	manifest map[string]any
	root     string
}

func newInstalledManifestFixture(t *testing.T) *installedManifestFixture {
	t.Helper()
	if UnsignedTestBuild {
		t.Skip("Windows policy identity belongs to ordinary desktop builds")
	}
	policy := newPolicyFixture(t)
	policy.config.Mode = "windows-policy"
	policy.config.UpdaterProtocol = 2
	policy.config.Platform = "windows"
	policy.config.Arch = "amd64"
	policy.config.ReleaseID = "windows-amd64-1.0.0"
	policy.config.SigningIdentity = "FIXTURE_WINDOWS_RELEASE"
	fixture := &installedManifestFixture{policy: policy, root: filepath.Join(t.TempDir(), "install")}
	var files []map[string]any
	for _, path := range []string{
		"LazyMind.exe", "LazyMindApp.exe", "resources/app.asar",
		"resources/runtime/bin/lazymind-update-helper.exe",
		"resources/runtime/bin/local-runtime-manager.exe",
		"resources/runtime/config/python-components.json",
	} {
		content := []byte("installed fixture: " + path)
		fixture.writeFile(t, path, content)
		files = append(files, fileManifestEntry(path, content))
	}
	fixture.manifest = map[string]any{
		"schemaVersion": 1, "kind": "windows-installed-manifest",
		"trustDomain": policy.config.TrustDomain, "audience": "official", "channel": "stable",
		"releaseId": "windows-amd64-1.1.0", "version": "1.1.0", "platform": "windows", "arch": "amd64",
		"files": files,
	}
	return fixture
}

func fileManifestEntry(path string, content []byte) map[string]any {
	hash := sha256.Sum256(content)
	return map[string]any{"path": path, "size": len(content), "sha256": hex.EncodeToString(hash[:])}
}

func (f *installedManifestFixture) writeFile(t *testing.T, relative string, content []byte) {
	t.Helper()
	path := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *installedManifestFixture) envelope(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	payload, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{
		"keyId": "key1", "payload": base64.StdEncoding.EncodeToString(payload),
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (f *installedManifestFixture) target(t *testing.T, envelope []byte) Release {
	t.Helper()
	descriptor := fileManifestEntry("installed-manifest.json", envelope)
	delete(descriptor, "path")
	descriptor["url"] = "https://updates.example.invalid/releases/windows-amd64-1.1.0/installed-manifest.json"
	raw, err := json.Marshal(map[string]any{
		"releaseId": "windows-amd64-1.1.0", "version": "1.1.0", "platform": "windows", "arch": "amd64",
		"signingIdentity": f.policy.config.SigningIdentity, "updaterProtocol": 2, "installedManifest": descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	var target Release
	if err := json.Unmarshal(raw, &target); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestWindowsInstalledManifestAcceptsTargetAndOfflineInstallation(t *testing.T) {
	f := newInstalledManifestFixture(t)
	raw := f.envelope(t, f.policy.private)
	target := f.target(t, raw)
	if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, f.policy.config, &target); err != nil {
		t.Fatalf("signed Windows target files failed verification: %v", err)
	}
	installed := f.policy.config
	installed.ReleaseID, installed.Version = target.ID, target.Version
	if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, installed, nil); err != nil {
		t.Fatalf("offline verification of the installed release failed: %v", err)
	}
}

func TestWindowsInstalledManifestOfflineVerificationRejectsWrongKeyAndChangedFile(t *testing.T) {
	f := newInstalledManifestFixture(t)
	installed := f.policy.config
	installed.ReleaseID, installed.Version = "windows-amd64-1.1.0", "1.1.0"
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyWindowsInstalledManifest(context.Background(), f.root, f.envelope(t, private), installed, nil); err == nil {
		t.Fatal("offline verification accepted a manifest signed by a different key")
	}
	f.writeFile(t, "resources/app.asar", []byte("INVALID!! fixture: resources/app.asar"))
	if err := VerifyWindowsInstalledManifest(context.Background(), f.root, f.envelope(t, f.policy.private), installed, nil); err == nil {
		t.Fatal("offline verification accepted a changed installed file")
	}
}

func TestWindowsInstalledManifestOfflineVerificationBindsInstalledIdentity(t *testing.T) {
	for _, change := range []struct{ field, value string }{
		{"releaseId", "windows-amd64-1.2.0"},
		{"version", "1.2.0"},
	} {
		t.Run(change.field, func(t *testing.T) {
			f := newInstalledManifestFixture(t)
			installed := f.policy.config
			installed.ReleaseID, installed.Version = "windows-amd64-1.1.0", "1.1.0"
			f.manifest[change.field] = change.value
			if err := VerifyWindowsInstalledManifest(t.Context(), f.root, f.envelope(t, f.policy.private), installed, nil); err == nil {
				t.Fatalf("offline verification accepted signed files with a different %s", change.field)
			}
		})
	}
}

func TestWindowsInstalledManifestRejectsWrongReleaseOrTrust(t *testing.T) {
	for _, field := range []string{"releaseId", "version", "trustDomain", "audience", "channel", "arch", "kind"} {
		t.Run(field, func(t *testing.T) {
			f := newInstalledManifestFixture(t)
			f.manifest[field] = "other"
			raw := f.envelope(t, f.policy.private)
			target := f.target(t, raw)
			if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, f.policy.config, &target); err == nil {
				t.Fatalf("accepted mismatch in %s despite valid signature and matching artifact hash", field)
			}
		})
	}
	t.Run("different signing key with matching artifact hash", func(t *testing.T) {
		f := newInstalledManifestFixture(t)
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw := f.envelope(t, private)
		target := f.target(t, raw)
		if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, f.policy.config, &target); err == nil {
			t.Fatal("accepted manifest from a non-pinned signing key")
		}
	})
	t.Run("validly signed payload with different policy digest", func(t *testing.T) {
		f := newInstalledManifestFixture(t)
		raw := f.envelope(t, f.policy.private)
		var ordered struct {
			Signature string `json:"signature"`
			Payload   string `json:"payload"`
			KeyID     string `json:"keyId"`
		}
		if err := json.Unmarshal(raw, &ordered); err != nil {
			t.Fatal(err)
		}
		alternate, err := json.Marshal(ordered)
		if err != nil || len(alternate) != len(raw) || bytes.Equal(alternate, raw) {
			t.Fatalf("expected equivalent envelope with different bytes but the same size: %v", err)
		}
		target := f.target(t, alternate)
		if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, f.policy.config, &target); err == nil {
			t.Fatal("accepted valid manifest with bytes different from the signed policy descriptor")
		}
	})
}

func TestWindowsInstalledManifestRejectsUnsafeOrChangedFiles(t *testing.T) {
	for _, change := range []string{"same-size content change", "path traversal with correct contents", "case alias with correct contents"} {
		t.Run(change, func(t *testing.T) {
			f := newInstalledManifestFixture(t)
			files := f.manifest["files"].([]map[string]any)
			switch change {
			case "same-size content change":
				f.writeFile(t, "LazyMind.exe", []byte("INVALID!! fixture: LazyMind.exe"))
			case "path traversal with correct contents":
				content := []byte("separate file outside the installation")
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.root), "outside.exe"), content, 0600); err != nil {
					t.Fatal(err)
				}
				f.manifest["files"] = append(files, fileManifestEntry("../outside.exe", content))
			case "case alias with correct contents":
				content := []byte("installed fixture: LazyMind.exe")
				f.writeFile(t, "lazymind.exe", content)
				f.manifest["files"] = append(files, fileManifestEntry("lazymind.exe", content))
			}
			raw := f.envelope(t, f.policy.private)
			target := f.target(t, raw)
			if err := VerifyWindowsInstalledManifest(context.Background(), f.root, raw, f.policy.config, &target); err == nil {
				t.Fatalf("accepted unsafe installed state: %s", change)
			}
		})
	}
}

func TestWindowsInstalledManifestRequiresExecutableAndElectronEntries(t *testing.T) {
	for _, name := range []string{"LazyMind.exe", "LazyMindApp.exe", "resources/app.asar"} {
		t.Run(name, func(t *testing.T) {
			f := newInstalledManifestFixture(t)
			var entries []map[string]any
			for _, file := range f.manifest["files"].([]map[string]any) {
				if file["path"] != name {
					entries = append(entries, file)
				}
			}
			f.manifest["files"] = entries
			raw := f.envelope(t, f.policy.private)
			target := f.target(t, raw)
			if err := VerifyWindowsInstalledManifest(t.Context(), f.root, raw, f.policy.config, &target); err == nil {
				t.Fatalf("accepted signed file list that omitted %s", name)
			}
		})
	}
}

func TestWindowsInstalledManifestRejectsSymbolicLinksWithExpectedBytes(t *testing.T) {
	for _, path := range []string{"LazyMind.exe", "resources"} {
		t.Run(path, func(t *testing.T) {
			f := newInstalledManifestFixture(t)
			original := filepath.Join(f.root, path)
			external := filepath.Join(filepath.Dir(f.root), "outside")
			if err := os.Rename(original, external); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, original); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("test account cannot create a Windows symbolic link: %v", err)
				}
				t.Fatal(err)
			}
			raw := f.envelope(t, f.policy.private)
			target := f.target(t, raw)
			if err := VerifyWindowsInstalledManifest(t.Context(), f.root, raw, f.policy.config, &target); err == nil {
				t.Fatal("accepted a link out of the installation root even though its bytes match")
			}
		})
	}
}
