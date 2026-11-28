package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
	"gopkg.in/yaml.v3"
)

func TestDesktopUpEnablesVerifiedOfflinePythonComponents(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		catalogURL, receipt  bool
		badReady, badVersion bool
		badRelease, badPin   bool
		receiptAlias         bool
		enabled              []string
	}{
		{name: "fixed package without a receipt", enabled: []string{"cluster", "rag"}},
		{name: "bad fixed package marker", badReady: true, enabled: []string{"cluster"}},
		{name: "different verified revision", badVersion: true, enabled: []string{"cluster"}},
		{name: "dangling fixed package receipt is not an absent receipt", receiptAlias: true, enabled: []string{"cluster"}},
		{name: "verified remote catalog receipt", catalogURL: true, receipt: true, enabled: []string{"cluster", "rag"}},
		{name: "remote catalog requires receipt", catalogURL: true},
		{name: "another release cannot reuse the previous selection", badRelease: true},
		{name: "changed catalog cannot reuse the previous selection", badPin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(desktopAppVersionEnvVar, "1.1.0")
			t.Setenv(ragEnabledEnvVar, "1")
			t.Setenv(localPortsPinnedEnvVar, "false")
			t.Setenv(modelCredentialKeyEnvVar, "offline-test-model-key")

			repo := t.TempDir()
			writeComposeFixture(t, repo)
			cfg, paths, err := NewRuntimeConfigWithOptions(RuntimeConfigOptions{
				Profile: "desktop", OwnerToken: "offline-test-owner", RepoRoot: repo,
				ResourcesRoot: filepath.Join(t.TempDir(), "runtime"), RuntimeRoot: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			resolveBefore := resolveRuntimeRecoveryRoot
			resolveRuntimeRecoveryRoot = func() (string, error) { return root, nil }
			t.Cleanup(func() { resolveRuntimeRecoveryRoot = resolveBefore })

			catalog := pythonComponentCatalog{SchemaVersion: 1, Platform: runtime.GOOS, Arch: runtime.GOARCH, Components: map[string]pythonComponent{}}
			selection := desktopupdate.SelectedComponents{
				ReleaseID: "offline-target", TransactionID: strings.Repeat("1", 32),
				Wanted: []string{"cluster", "rag"}, VerifiedComponents: map[string]string{},
			}
			sites := map[string]string{}
			for _, id := range selection.Wanted {
				entry := pythonComponent{
					ID: id, Revision: strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64), BaseFingerprint: strings.Repeat("c", 64),
					Tag: strings.Repeat("d", 64), Platform: runtime.GOOS, Arch: runtime.GOARCH,
					PythonABI: "cp311", Packages: map[string]string{"fixture-" + id: "1"},
				}
				if tc.catalogURL {
					entry.CatalogURL = "https://offline.invalid/" + id + "/python-components.json"
				}
				catalog.Components[id] = entry
				selection.VerifiedComponents[id] = entry.Revision
				base := filepath.Join(paths.RuntimeRoot, "deps", "python-components", id)
				dir := filepath.Join(base, entry.Revision)
				sites[id] = filepath.Join(dir, "site-packages")
				replacementFile(t, filepath.Join(sites[id], "fixture.py"), []byte("VALUE = True\n"))
				replacementFile(t, filepath.Join(dir, "bundle-manifest.json"), replacementJSON(t, entry))
				ready := entry.SHA256
				if id == "rag" && tc.badReady {
					ready = strings.Repeat("f", 64)
				}
				replacementFile(t, filepath.Join(dir, ".ready"), []byte(ready+"\n"))
				if id == "rag" && tc.badVersion {
					selection.VerifiedComponents[id] = strings.Repeat("e", 64)
				}
				receipt := filepath.Join(base, "catalog-"+entry.Tag+".json")
				if tc.receiptAlias && id == "rag" {
					if err := os.Symlink(filepath.Join(base, "missing-receipt"), receipt); err != nil {
						const windowsPrivilegeNotHeld = syscall.Errno(1314)
						if os.IsPermission(err) || errors.Is(err, errors.ErrUnsupported) ||
							(runtime.GOOS == "windows" && errors.Is(err, windowsPrivilegeNotHeld)) {
							t.Skipf("symlink fixture unavailable: %v", err)
						}
						t.Fatal(err)
					}
				} else if tc.receipt {
					replacementFile(t, receipt, replacementJSON(t, entry))
				}
			}
			// This fixture represents the previously accepted packaged catalog
			// and release identity, without an in-progress update transaction.
			raw := replacementJSON(t, catalog)
			hash := sha256.Sum256(raw)
			selection.ComponentsSHA256 = hex.EncodeToString(hash[:])
			if tc.badPin {
				raw = append(raw, '\n')
			}
			release := selection.ReleaseID
			if tc.badRelease {
				release = "another-release"
			}
			replacementFile(t, filepath.Join(paths.ResourcesRoot, "config", "python-components.json"), raw)
			replacementFile(t, filepath.Join(filepath.Dir(paths.ResourcesRoot), "app-update.yml"),
				replacementJSON(t, map[string]any{"lazymind": map[string]string{"releaseId": release}}))
			if err := desktopupdate.SaveSelectedComponents(paths.RuntimeRoot, selection); err != nil {
				t.Fatal(err)
			}

			runner := &fakeRunner{t: t}
			manager := NewRuntimeManager(runner, "fixture-manager")
			manager.SetOutput(io.Discard, io.Discard)
			manager.pollInterval = 10 * time.Millisecond
			manager.processScanner = func(RuntimePaths) ([]LocalProcessRecord, error) { return nil, nil }
			ready := func(int, time.Duration) bool { return true }
			manager.probeAPI, manager.probeSQLiteServer = ready, ready
			manager.probeLocalProxy, manager.probeFrontend = ready, ready
			manager.probeAuth, manager.probeChannelGateway = ready, ready
			manager.probeCore, manager.probeScan, manager.probeFileWatch = ready, ready, ready
			manager.waitHostReady = func(context.Context, RuntimeConfig, []AlgorithmServiceSpec) error { return nil }

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			done := make(chan struct{})
			var upErr error
			go func() {
				defer close(done)
				upErr = manager.Up(ctx, cfg, paths)
			}()
			defer func() { cancel(); <-done }()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			var state RuntimeState
			for {
				state, err = readRuntimeState(paths.StateFile)
				if err == nil && state.OverallStatus == "ready" {
					break
				}
				select {
				case <-done:
					t.Fatalf("Desktop up exited before reaching ready: %v", upErr)
				case <-ctx.Done():
					t.Fatalf("Desktop up did not become ready: %v", ctx.Err())
				case <-tick.C:
				}
			}
			// The supervisor starts the real internal service subcommands
			// against these same paths. Each then builds the child's environment.
			generated, err := os.ReadFile(paths.GeneratedConfig)
			if err != nil {
				t.Fatal(err)
			}
			var compose processComposeConfig
			if err := yaml.Unmarshal(generated, &compose); err != nil {
				t.Fatal(err)
			}
			for name, command := range map[string]string{coreProcessName: "internal core-run", chatProcessName: "internal algorithm-run --service " + chatProcessName} {
				process, ok := compose.Processes[name]
				if !ok || !strings.Contains(process.Command, command) ||
					!slices.Contains(process.Environment, runtimeRootEnvVar+"="+paths.RuntimeRoot) ||
					!slices.Contains(process.Environment, runtimeResourcesRootEnvVar+"="+paths.ResourcesRoot) {
					t.Fatalf("Desktop up did not configure %s against the current runtime and resources roots", name)
				}
			}
			started := applyStateConfig(cfg, state)
			for service, env := range map[string][]string{
				"Core": mustCoreServiceEnv(t, started, paths),
				"Chat": algorithmServiceEnv(started, paths, chatProcessName),
			} {
				var wantPaths []string
				for _, id := range pythonComponentIDs {
					enabled := "0"
					if slices.Contains(tc.enabled, id) {
						enabled = "1"
						wantPaths = append(wantPaths, sites[id])
					}
					current := "LAZYMIND_" + strings.ToUpper(id) + "_ENABLED=" + enabled
					if !slices.Contains(env, current) {
						t.Errorf("%s did not set %s", service, current)
					}
				}
				if !slices.Contains(env, "LAZYMIND_PYTHON_COMPONENT_PATHS="+strings.Join(wantPaths, string(os.PathListSeparator))) {
					t.Errorf("%s did not activate exactly these component paths: %v", service, wantPaths)
				}
			}
			state.OverallStatus = "stopped"
			if err := writeRuntimeState(paths.StateFile, state); err != nil {
				t.Fatal(err)
			}
			<-done
			if upErr != nil {
				t.Fatalf("Desktop up did not complete: %v", upErr)
			}
			if len(runner.calls) != 1 || !slices.Contains(runner.calls[0].Args, "up") {
				t.Fatalf("fixture unexpectedly ran a command besides the supervisor: %v", runner.calls)
			}
		})
	}
}
