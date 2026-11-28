//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const recoveryLaunchMarker = "LAZYMIND_RECOVERY_TEST_CHILD_MARKER"

func init() {
	marker := os.Getenv(recoveryLaunchMarker)
	if marker == "" || !strings.EqualFold(filepath.Base(os.Args[0]), "LazyMindApp.exe") {
		return
	}
	state := "unverified"
	if record, err := ReadNativeRecord(os.Getenv("LAZYMIND_RECOVERY_TEST_UPDATER")); err == nil && record != nil && record.WindowsInstallVerified {
		state = "verified"
	}
	if err := os.WriteFile(marker, []byte(state), 0600); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func buildWindowsRecoveryLauncher(t *testing.T, executable string, c Config) {
	t.Helper()
	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	initFile := filepath.Join(work, "recovery-fixture-init.go")
	// Substitute only OS lookup results. The real Windows main retains its
	// public startup guard, file verification and process creation.
	writeHandoffProcessFile(t, initFile, []byte(`package main
import (
	"context"
	"os"
)
func init() {
	resolveLauncherUpdaterRoot = func() (string, error) { return os.Getenv("LAZYMIND_RECOVERY_TEST_UPDATER"), nil }
	readLauncherWindowsBootID = func(context.Context) (string, error) { return os.Getenv("LAZYMIND_RECOVERY_TEST_BOOT"), nil }
}`))
	overlay := filepath.Join(work, "launcher-overlay.json")
	virtual := filepath.Join(module, "cmd", "desktop-launcher", "fixture_recovery_os.go")
	writeHandoffProcessFile(t, overlay, nativeJSON(t, map[string]any{"Replace": map[string]string{virtual: initFile}}))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	flags := "-X main.embeddedConfig=" + base64.StdEncoding.EncodeToString(nativeJSON(t, c))
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-overlay", overlay, "-ldflags", flags, "-o", executable, "./cmd/desktop-launcher")
	cmd.Dir = module
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build public recovery launcher: %v %s", err, output)
	}
}

func TestWindowsPolicyPublicLauncherVerifiesRecoveryBeforeCreatingAppProcess(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("official Windows policy targets amd64")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"verified reinstall", "corrupt install", "argument cannot impersonate new OS boot"} {
		t.Run(name, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "repair")
			r := makeWindowsRecoveryRecord(t, f)
			config := f.policy.config
			config.ReleaseID, config.Version = r.TargetReleaseID, r.TargetVersion
			launcher := filepath.Join(f.disk.root, "LazyMind.exe")
			buildWindowsRecoveryLauncher(t, launcher, config)
			f.disk.writeFile(t, "LazyMindApp.exe", child)
			manifest := signedHandoffProcessTree(t, f.disk, slices.Clone(f.disk.manifest["files"].([]map[string]any)))
			entry := windowsPolicyManifestArtifact(r.TargetReleaseID)
			entry.SHA256 = fileManifestEntry("installed-manifest.json", manifest)["sha256"].(string)
			entry.Size = int64(len(manifest))
			f.policy.policy["releases"].([]any)[1].(map[string]any)["installedManifest"] = entry
			// The production launcher uses its real clock. Refresh the fixture
			// envelope without altering the existing transaction's source data.
			now := time.Now().UTC()
			f.policy.policy["issuedAt"] = now.Add(-time.Minute).Format(time.RFC3339)
			f.policy.policy["expiresAt"] = now.Add(time.Hour).Format(time.RFC3339)
			f.policy.policy["policySequence"] = r.PolicySequence + 1
			decision, err := Check(t.Context(), f.policy.envelope(t), f.policy.config, f.policy.state, f.native.root, false, now)
			if err != nil || decision.Next == nil || decision.Next.ID != r.TargetReleaseID {
				t.Fatalf("authorize exact launcher bytes: %+v, %v", decision, err)
			}
			r.PolicySequence, r.PolicyDigest = decision.PolicySequence, decision.PolicyDigest
			r.InstalledManifestSHA256, r.InstalledManifestSize = entry.SHA256, entry.Size
			r.InstalledManifestPath = filepath.Join(DownloadCacheDirectory(f.native.root, f.policy.config), entry.SHA256+".json")
			if err := writeNativeRecord(f.native.root, r); err != nil {
				t.Fatal(err)
			}
			boot := strings.Repeat("b", 64)
			var args []string
			var wantFailure string
			switch name {
			case "corrupt install":
				f.disk.writeFile(t, "resources/app.asar", []byte("unsigned replacement"))
				wantFailure = "installed_manifest_file"
			case "argument cannot impersonate new OS boot":
				args = []string{"--boot-id=" + boot}
				boot, wantFailure = r.BootID, "handoff_restart_required"
			}
			work := t.TempDir()
			marker := filepath.Join(work, "launched-app")
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, launcher, args...)
			command.Env = append(os.Environ(), recoveryLaunchMarker+"="+marker, "LAZYMIND_RECOVERY_TEST_UPDATER="+f.native.root,
				"LAZYMIND_RECOVERY_TEST_BOOT="+boot, "LOCALAPPDATA="+work)
			if wantFailure == "" {
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("valid reinstalled target failed at public entry: %v %s", err, out)
				}
				if raw, err := os.ReadFile(marker); err != nil || string(raw) != "verified" {
					t.Fatalf("public entry did not start B after saving proof: %q, %v", raw, err)
				}
				return
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Wait()
			defer cancel() // Close the real launcher's error dialog without touching any other process.
			log := filepath.Join(work, "LazyMind", "Logs", "desktop", "desktop-launcher.log")
			if err := pollHandoffProcess(ctx, func() bool {
				raw, _ := os.ReadFile(log)
				return strings.Contains(string(raw), wantFailure)
			}); err != nil {
				raw, _ := os.ReadFile(log)
				t.Fatalf("public launcher did not report %s: %s, %v", wantFailure, raw, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("public launcher executed an app without repair proof: %v", err)
			}
		})
	}
}
