//go:build !lazymind_unsigned_test

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
)

func installPythonPayloadIdentityProbe(t *testing.T, resources string, config desktopupdate.Config) string {
	t.Helper()
	root := t.TempDir()
	old := resolveRuntimeRecoveryRoot
	resolveRuntimeRecoveryRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { resolveRuntimeRecoveryRoot = old })
	if err := os.MkdirAll(filepath.Join(resources, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]any{"status": "enabled", "config": config})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "verified-identity")
	source := filepath.Join(root, "identity.go")
	code := `package main
import "os"
func main() {
	if len(os.Args) != 2 || os.Args[1] != "verify-identity" { os.Exit(1) }
	if os.WriteFile(` + strconv.Quote(marker) + `, []byte("verified"), 0600) != nil { os.Exit(1) }
	os.Stdout.Write([]byte(` + strconv.Quote(string(output)) + `))
}`
	if err := os.WriteFile(source, []byte(code), 0644); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(resources, "bin", executableName("lazymind-update-helper"))
	if output, err := exec.CommandContext(t.Context(), executablePath(filepath.Join(runtime.GOROOT(), "bin"), "go"), "build", "-o", helper, source).CombinedOutput(); err != nil {
		t.Fatalf("build fixture identity provider: %v %s", err, output)
	}
	return marker
}

func pythonPayloadIdentityConfig(mode string, publicKey []byte) desktopupdate.Config {
	return desktopupdate.Config{
		Mode: mode, TrustDomain: "fixture-desktop", Audience: "official", Channel: "stable",
		ReleaseID: "source", Version: "1.0.0", Platform: "windows", Arch: "amd64", SigningIdentity: "fixture",
		UpdaterProtocol: 2, PolicyURL: "https://updates.example.invalid/policy.json", AllowedOrigins: []string{"https://updates.example.invalid"},
		PublicKeys: map[string]string{"fixture": base64.StdEncoding.EncodeToString(publicKey)},
	}
}

func TestWindowsPolicySignedPythonPayloadSurvivesFirstStartup(t *testing.T) {
	app := filepath.Join(t.TempDir(), "LazyMind installed")
	resources := filepath.Join(app, "resources", "runtime")
	if err := os.MkdirAll(resources, 0755); err != nil {
		t.Fatal(err)
	}
	const interpreter = "runtimes/python/cpython/python.exe"
	const venv = "deps/python/auth-service/pyvenv.cfg"
	writePythonPayloadArchive(t, resources, map[string]string{
		interpreter: "python fixture bytes", venv: "home = original-installation\n",
	})
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := pythonPayloadIdentityConfig("windows-policy", publicKey)
	identityProbe := installPythonPayloadIdentityProbe(t, resources, config)
	const archiveEntry = "resources/runtime/python-runtime.zip"
	entries := []map[string]any{}
	helper := "resources/runtime/bin/" + executableName("lazymind-update-helper")
	files := []string{
		"LazyMind.exe", "LazyMindApp.exe", "resources/app.asar",
		"resources/runtime/bin/lazymind-update-helper.exe",
		"resources/runtime/bin/local-runtime-manager.exe",
		"resources/runtime/config/python-components.json", archiveEntry,
	}
	if runtime.GOOS != "windows" {
		files = append(files, helper)
	}
	for _, path := range files {
		filename := filepath.Join(app, filepath.FromSlash(path))
		if path != archiveEntry && path != helper {
			if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, []byte("static fixture bytes for "+path), 0644); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		entries = append(entries, map[string]any{"path": path, "size": len(raw), "sha256": hex.EncodeToString(hash[:])})
	}
	payload, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "kind": "windows-installed-manifest", "trustDomain": config.TrustDomain,
		"audience": config.Audience, "channel": config.Channel, "releaseId": config.ReleaseID,
		"version": config.Version, "platform": config.Platform, "arch": config.Arch, "files": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]string{
		"keyId": "fixture", "payload": base64.StdEncoding.EncodeToString(payload),
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(app, "resources", "installed-manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
		t.Fatal(err)
	}
	verify := func() error {
		onDisk, err := os.ReadFile(manifestPath)
		if err != nil {
			return err
		}
		return desktopupdate.VerifyWindowsInstalledManifest(t.Context(), app, onDisk, config, nil)
	}
	if err := verify(); err != nil {
		t.Fatalf("fixture identity before first startup: %v", err)
	}
	paths := RuntimePaths{ResourcesRoot: resources}
	if err := prepareBundledPythonRuntime(t.Context(), paths, nil); err != nil {
		t.Fatalf("first startup failed to expand the signed runtime: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(resources, filepath.FromSlash(interpreter))); err != nil || string(raw) != "python fixture bytes" {
		t.Fatalf("signed Python was not installed: %q %v", raw, err)
	}
	if err := verify(); err != nil {
		_, archiveErr := os.Stat(filepath.Join(resources, pythonPayloadArchiveName))
		t.Fatalf("first startup invalidated the signed installation: %v; archive: %v", err, archiveErr)
	}
	if _, err := os.Stat(identityProbe); err != nil {
		t.Fatalf("Python preparation did not establish the installed update mode: %v", err)
	}
	// Relocation legitimately edits the expanded venv file. Restart must use
	// the existing activation marker instead of overwriting the relocated file.
	venvPath := filepath.Join(resources, filepath.FromSlash(venv))
	if err := os.WriteFile(venvPath, []byte("home = repaired-installation\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareBundledPythonRuntime(t.Context(), paths, func(p pythonPayloadProgress) {
		t.Errorf("restart extracted the already installed runtime: %s", p.Stage)
	}); err != nil {
		t.Fatalf("second startup: %v", err)
	}
	if raw, err := os.ReadFile(venvPath); err != nil || !bytes.Equal(raw, []byte("home = repaired-installation\n")) {
		t.Fatalf("restart rewrote the relocated venv: %q %v", raw, err)
	}
	if err := verify(); err != nil {
		t.Fatalf("second startup invalidated the signed installation: %v", err)
	}
	archive := filepath.Join(resources, pythonPayloadArchiveName)
	tampered, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	tampered[0] ^= 1
	if err := os.WriteFile(archive, tampered, 0644); err != nil {
		t.Fatal(err)
	}
	if err := verify(); desktopupdate.ErrorCode(err) != "installed_manifest_file" {
		t.Fatalf("changed archive was not rejected by offline identity: %v", err)
	}
}

func TestLegacyPythonPayloadRemovesArchiveWithStaleInstalledManifest(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "resources")
	resources := filepath.Join(parent, "runtime")
	if err := os.MkdirAll(resources, 0755); err != nil {
		t.Fatal(err)
	}
	config := pythonPayloadIdentityConfig("official", make([]byte, ed25519.PublicKeySize))
	installPythonPayloadIdentityProbe(t, resources, config)
	if err := os.WriteFile(filepath.Join(parent, "installed-manifest.json"), []byte("stale manifest from a different installation"), 0644); err != nil {
		t.Fatal(err)
	}
	writePythonPayloadArchive(t, resources, map[string]string{
		"runtimes/python/cpython/python.exe":  "python fixture bytes",
		"deps/python/auth-service/pyvenv.cfg": "home = old-installation\n",
	})
	if err := prepareBundledPythonRuntime(t.Context(), RuntimePaths{ResourcesRoot: resources}, nil); err != nil {
		t.Fatalf("ordinary installer failed to expand Python: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resources, pythonPayloadArchiveName)); !os.IsNotExist(err) {
		t.Fatalf("stale manifest changed ordinary archive cleanup: %v", err)
	}
}
