//go:build !lazymind_unsigned_test

package main

import (
	"context"
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
	"testing"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
)

type windowsPolicyIdentityFixture struct {
	root, helper, manifest string
	config                 desktopupdate.Config
	key                    ed25519.PrivateKey
}

var windowsPolicyIdentityFiles = []string{
	"LazyMind.exe", "LazyMindApp.exe", "resources/app.asar",
	"resources/runtime/bin/lazymind-update-helper.exe",
	"resources/runtime/bin/local-runtime-manager.exe",
	"resources/runtime/config/python-components.json",
}

func newWindowsPolicyIdentityFixture(t *testing.T) windowsPolicyIdentityFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "LazyMind 安装")
	f := windowsPolicyIdentityFixture{
		root:     root,
		helper:   filepath.Join(root, "resources", "runtime", "bin", "lazymind-update-helper.exe"),
		manifest: filepath.Join(root, "resources", "installed-manifest.json"),
		key:      privateKey,
		config: desktopupdate.Config{
			Mode: "windows-policy", TrustDomain: "windows-handoff-fixture", Audience: "official", Channel: "stable",
			Platform: "windows", Arch: "amd64", ReleaseID: "source", Version: "1.0.0",
			SigningIdentity: "LAZYMIND_OFFICIAL_RELEASE", UpdaterProtocol: 2,
			PolicyURL: "https://updates.example.invalid/policy.json", AllowedOrigins: []string{"https://updates.example.invalid"},
			PublicKeys: map[string]string{"fixture": base64.StdEncoding.EncodeToString(publicKey)},
		},
	}
	for _, name := range windowsPolicyIdentityFiles {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		writeWindowsIdentityTestFile(t, file, []byte("fixture: "+name))
	}
	return f
}

func writeWindowsIdentityTestFile(t *testing.T, file string, data []byte) {
	t.Helper()
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f windowsPolicyIdentityFixture) sign(t *testing.T, release, version string, key ed25519.PrivateKey) {
	t.Helper()
	var entries []map[string]any
	for _, name := range windowsPolicyIdentityFiles {
		data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		entries = append(entries, map[string]any{"path": name, "size": len(data), "sha256": hex.EncodeToString(hash[:])})
	}
	payload, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "kind": "windows-installed-manifest", "trustDomain": f.config.TrustDomain,
		"audience": f.config.Audience, "channel": f.config.Channel, "platform": f.config.Platform,
		"arch": f.config.Arch, "releaseId": release, "version": version, "files": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]string{
		"keyId": "fixture", "payload": base64.StdEncoding.EncodeToString(payload),
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	writeWindowsIdentityTestFile(t, f.manifest, envelope)
}

func TestWindowsPolicyInstalledIdentityFixtureVerifies(t *testing.T) {
	f := newWindowsPolicyIdentityFixture(t)
	f.sign(t, f.config.ReleaseID, f.config.Version, f.key)
	manifest, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := desktopupdate.VerifyWindowsInstalledManifest(t.Context(), f.root, manifest, f.config, nil); err != nil {
		t.Fatalf("invalid signed fixture: %v", err)
	}
}

func TestWindowsPolicyInstalledIdentityFromPackagedHelper(t *testing.T) {
	for _, kind := range []string{"valid offline", "changed app", "another release", "another key", "missing manifest", "copied helper"} {
		t.Run(kind, func(t *testing.T) {
			f := newWindowsPolicyIdentityFixture(t)
			f.sign(t, f.config.ReleaseID, f.config.Version, f.key)
			// Exercise a known good path before changing exactly one input.
			if err := verifyWindowsPolicyInstalledIdentity(t.Context(), f.helper, f.config); err != nil {
				t.Fatalf("offline installed identity did not verify: %v", err)
			}
			if kind == "valid offline" {
				return
			}
			helper := f.helper
			switch kind {
			case "changed app":
				writeWindowsIdentityTestFile(t, filepath.Join(f.root, "resources", "app.asar"), []byte("CHANGED: resources/app.asar"))
			case "another release":
				f.sign(t, "target", "1.1.0", f.key)
			case "another key":
				_, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				f.sign(t, f.config.ReleaseID, f.config.Version, key)
			case "missing manifest":
				if err := os.Remove(f.manifest); err != nil {
					t.Fatal(err)
				}
			case "copied helper":
				helper = filepath.Join(filepath.Dir(f.helper), "helper-copy.exe")
				data, err := os.ReadFile(f.helper)
				if err != nil {
					t.Fatal(err)
				}
				writeWindowsIdentityTestFile(t, helper, data)
			}
			if err := verifyWindowsPolicyInstalledIdentity(t.Context(), helper, f.config); err == nil {
				t.Fatal("offered installed identity was accepted")
			}
		})
	}
}

func TestWindowsPolicyVerifyIdentityCLI(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("the packaged helper identity entrypoint requires native Windows x64")
	}
	f := newWindowsPolicyIdentityFixture(t)
	raw, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	// The production helper is built with test-generated Ed25519 trust keys.
	// This PE has no Authenticode signature and uses the real command dispatcher.
	build, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(build, filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build",
		"-ldflags=-X main.embeddedConfig="+base64.StdEncoding.EncodeToString(raw), "-o", f.helper, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build packaged helper: %v %s", err, output)
	}
	f.sign(t, f.config.ReleaseID, f.config.Version, f.key)
	output, err := exec.CommandContext(t.Context(), f.helper, "verify-identity").Output()
	var result struct {
		Status string
		Config desktopupdate.Config
	}
	if err != nil || json.Unmarshal(output, &result) != nil || result.Status != "enabled" || result.Config.ReleaseID != f.config.ReleaseID {
		t.Fatalf("packaged verify-identity rejected the signed offline installation: %v %s", err, output)
	}
	writeWindowsIdentityTestFile(t, filepath.Join(f.root, "resources", "app.asar"), []byte("CHANGED: resources/app.asar"))
	if output, err := exec.CommandContext(t.Context(), f.helper, "verify-identity").Output(); err == nil {
		t.Fatalf("packaged verify-identity accepted a replaced app.asar: %s", output)
	}
}
