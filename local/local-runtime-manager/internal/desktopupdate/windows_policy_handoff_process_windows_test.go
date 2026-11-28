//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These process roles are used only by copies of this test executable. The
// updater itself is the real cmd/update-helper built from the production code.
const (
	handoffChildEnv        = "LAZYMIND_3B2_PROCESS_FIXTURE"
	handoffInstallEnv      = "LAZYMIND_3B2_HKCU_INSTALL_LOCATION"
	handoffUserProgramsEnv = "LAZYMIND_3B2_USER_PROGRAM_FILES"
)

type handoffProcessFixturePaths struct {
	Base, App, StagedB, Transaction string
}

func init() {
	setup := os.Getenv(handoffChildEnv)
	if setup == "" {
		return
	}
	var fixture handoffProcessFixturePaths
	raw, err := os.ReadFile(setup)
	if err == nil {
		err = json.Unmarshal(raw, &fixture)
	}
	if err == nil {
		err = runHandoffFixtureProcess(fixture)
	}
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func runHandoffFixtureProcess(f handoffProcessFixturePaths) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	marker := func(name string) string { return filepath.Join(f.Base, name) }
	wait := func(name string) error {
		return pollHandoffProcess(ctx, func() bool { _, err := os.Stat(marker(name)); return err == nil })
	}
	switch strings.ToLower(filepath.Base(os.Args[0])) {
	case "lazymindapp.exe":
		if err := wait("begin-handoff"); err != nil {
			return err
		}
		helper := filepath.Join(f.App, "resources", "runtime", "bin", "lazymind-update-helper.exe")
		cmd := exec.CommandContext(ctx, helper, "windows-handoff", f.Transaction)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return wait("allow-A-exit")
	case "lazymind.exe":
		file, err := os.OpenFile(marker("B-launches"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = file.WriteString("launched\n")
		return err
	default:
		if !slices.Equal(os.Args[1:], []string{"--updated", "/S", "--simple-install", "/currentuser"}) {
			return errors.New("unexpected arguments for the installer fixture")
		}
		if err := os.WriteFile(marker("NSIS-started"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			return err
		}
		record, err := ReadNativeRecord(filepath.Join(f.Base, "LazyMindUpdater"))
		if err != nil || record == nil || record.SourceActor == nil {
			return errors.New("installer started without a recorded source process")
		}
		if alive, err := SameProcess(*record.SourceActor); err != nil || alive {
			return errors.New("installer started before the recorded source process exited")
		}
		// Model setInstallModePerUser's registry-first destination, even when
		// the executable being updated was launched from another directory.
		destination := os.Getenv(handoffInstallEnv)
		if destination == "" {
			destination = filepath.Join(os.Getenv(handoffUserProgramsEnv), "LazyMind")
		}
		err = filepath.WalkDir(f.StagedB, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(f.StagedB, name)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(destination, relative), data, 0600)
		})
		if err != nil {
			return err
		}
		if err := os.WriteFile(marker("B-files-written"), nil, 0600); err != nil {
			return err
		}
		// Correct files already exist. The owner must still wait for our exit.
		return wait("allow-NSIS-exit")
	}
}

func pollHandoffProcess(ctx context.Context, ready func() bool) error {
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func writeHandoffProcessFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func buildHandoffFixtureHelper(t *testing.T, base, executable string, c Config) {
	t.Helper()
	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Substitute Windows account inputs only. The product still resolves its
	// installer destination, compares paths and dispatches the real command.
	initSource := filepath.Join(base, "handoff-fixture-init.go")
	writeHandoffProcessFile(t, initSource, []byte(`package main
import "os"
func init() {
	resolveRecoveryBase = func() (string, error) { return os.Getenv("LAZYMIND_3B2_RECOVERY_BASE"), nil }
	readWindowsPolicyInstallLocation = func() (string, error) { return os.Getenv("LAZYMIND_3B2_HKCU_INSTALL_LOCATION"), nil }
	readWindowsPolicyUserProgramFiles = func() (string, error) { return os.Getenv("LAZYMIND_3B2_USER_PROGRAM_FILES"), nil }
}`))
	overlay := filepath.Join(base, "build-overlay.json")
	virtual := filepath.Join(module, "cmd", "update-helper", "fixture_handoff_account_root.go")
	writeHandoffProcessFile(t, overlay, nativeJSON(t, map[string]any{"Replace": map[string]string{virtual: initSource}}))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	flags := "-X main.embeddedConfig=" + base64.StdEncoding.EncodeToString(nativeJSON(t, c))
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-overlay", overlay, "-ldflags", flags, "-o", executable, "./cmd/update-helper")
	cmd.Dir = module
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build packaged updater: %v %s", err, output)
	}
}

func signedHandoffProcessTree(t *testing.T, disk *installedManifestFixture, files []map[string]any) []byte {
	t.Helper()
	for i, entry := range files {
		name := entry["path"].(string)
		raw, err := os.ReadFile(filepath.Join(disk.root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files[i] = fileManifestEntry(name, raw)
	}
	disk.manifest["files"] = files
	raw := disk.envelope(t, disk.policy.private)
	disk.writeFile(t, "resources/installed-manifest.json", raw)
	return raw
}

func prepareNativeHandoffProcess(t *testing.T, f windowsPolicyInstallProofFixture, base, app, root string, image []byte) *NativeRecord {
	t.Helper()
	c := f.policy.config
	installationRoot := filepath.Join(app, "resources", "runtime")
	diskA := &installedManifestFixture{policy: f.policy, root: app, manifest: make(map[string]any)}
	for key, value := range f.disk.manifest {
		diskA.manifest[key] = value
	}
	diskA.manifest["releaseId"], diskA.manifest["version"] = c.ReleaseID, c.Version
	files := f.disk.manifest["files"].([]map[string]any)
	for _, entry := range files {
		diskA.writeFile(t, entry["path"].(string), []byte("A fixture"))
	}
	diskA.writeFile(t, "LazyMindApp.exe", image)
	buildHandoffFixtureHelper(t, base, filepath.Join(installationRoot, "bin", "lazymind-update-helper.exe"), c)
	sourceManifest := signedHandoffProcessTree(t, diskA, slices.Clone(files))
	f.disk.writeFile(t, "LazyMind.exe", image)
	targetManifest := signedHandoffProcessTree(t, f.disk, slices.Clone(files))

	record, err := ReadNativeRecord(f.native.root)
	if err != nil || record == nil {
		t.Fatalf("read fixture intent: %+v, %v", record, err)
	}
	catalog, err := os.ReadFile(record.ComponentsPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := DownloadCacheDirectory(root, c)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	installerHash := fileManifestEntry("installer.exe", image)["sha256"].(string)
	manifestHash := fileManifestEntry("installed-manifest.json", targetManifest)["sha256"].(string)
	releases := f.policy.policy["releases"].([]any)
	for i, raw := range [][]byte{sourceManifest, targetManifest} {
		release := releases[i].(map[string]any)
		artifact := windowsPolicyManifestArtifact(release["releaseId"].(string))
		artifact.SHA256 = fileManifestEntry("installed-manifest.json", raw)["sha256"].(string)
		artifact.Size = int64(len(raw))
		release["installedManifest"] = artifact
	}
	archive := releases[1].(map[string]any)["package"].(map[string]any)
	archive["sha256"], archive["size"], archive["unpackedBytes"] = installerHash, len(image), len(image)*3
	now := time.Now().UTC()
	f.policy.policy["issuedAt"], f.policy.policy["expiresAt"] = now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)
	decision, err := Check(t.Context(), f.policy.envelope(t), c, f.policy.state, root, false, now)
	if err != nil || decision.Next == nil || decision.Next.ID != record.TargetReleaseID {
		t.Fatalf("accept executable fixture target: %+v %v", decision, err)
	}
	record.InstallationRoot, record.Scope.Installation = installationRoot, installationRoot
	record.PolicyDigest, record.PolicySequence = decision.PolicyDigest, decision.PolicySequence
	record.PackagePath, record.PackageSHA256, record.PackageSize = filepath.Join(cache, installerHash+".exe"), installerHash, int64(len(image))
	record.PackageUnpackedBytes = decision.Next.Package.UnpackedBytes
	record.ComponentsPath = filepath.Join(cache, record.ComponentsSHA256+".json")
	record.InstalledManifestPath, record.InstalledManifestSHA256, record.InstalledManifestSize = filepath.Join(cache, manifestHash+".json"), manifestHash, int64(len(targetManifest))
	writeHandoffProcessFile(t, record.PackagePath, image)
	writeHandoffProcessFile(t, record.ComponentsPath, catalog)
	writeHandoffProcessFile(t, record.InstalledManifestPath, targetManifest)
	return record
}

func TestWindowsPolicyPackagedHandoffWaitsForInstallerExit(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("official Windows policy targets amd64")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name             string
		appPath          string
		registryLocation string
		userPrograms     string
		wantHandoff      bool
	}{
		{"matching registered installation", "Programs/LazyMind", "Programs/LazyMind", "Redirected Programs", true},
		{"matching known-folder installation", "Programs/LazyMind", "", "Programs", true},
		{"registry points to another installation", "Programs/LazyMind", "Other Programs/LazyMind", "Programs", false},
		{"known folder points to another installation", "Programs/LazyMind", "", "Redirected Programs", false},
		{"portable copy outside installation target", "portable/LazyMind", "Programs/LazyMind", "Programs", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "installing")
			base := filepath.Join(t.TempDir(), "Local AppData")
			root := filepath.Join(base, "LazyMindUpdater")
			app := filepath.Join(base, filepath.FromSlash(scenario.appPath))
			location := ""
			if scenario.registryLocation != "" {
				location = filepath.Join(base, filepath.FromSlash(scenario.registryLocation))
			}
			record := prepareNativeHandoffProcess(t, f, base, app, root, image)
			marker := func(name string) string { return filepath.Join(base, name) }
			setup := marker("process-fixture.json")
			writeHandoffProcessFile(t, setup, nativeJSON(t, handoffProcessFixturePaths{base, app, f.disk.root, record.TransactionID}))
			stdout, err := os.Create(marker("handoff-output.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			source := exec.CommandContext(ctx, filepath.Join(app, "LazyMindApp.exe"))
			source.Stdout, source.Stderr = stdout, stdout
			source.Env = append(os.Environ(), handoffChildEnv+"="+setup, "LAZYMIND_3B2_RECOVERY_BASE="+base,
				handoffInstallEnv+"="+location, handoffUserProgramsEnv+"="+filepath.Join(base, scenario.userPrograms))
			if err := source.Start(); err != nil {
				t.Fatal(err)
			}
			sourceExit := make(chan error, 1)
			go func() { sourceExit <- source.Wait() }()
			t.Cleanup(func() {
				_ = os.WriteFile(marker("allow-A-exit"), nil, 0600)
				_ = os.WriteFile(marker("allow-NSIS-exit"), nil, 0600)
			})
			actor, err := IdentifyProcess(source.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			sourceHandle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(actor.PID))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(sourceHandle)
			record.SourceActor = &actor
			if err := writeNativeRecord(root, record); err != nil {
				t.Fatal(err)
			}
			writeHandoffProcessFile(t, marker("begin-handoff"), nil)
			var reply struct{ Status, TransactionID, Reason string }
			if err := pollHandoffProcess(ctx, func() bool {
				raw, _ := os.ReadFile(stdout.Name())
				line, _, complete := bytes.Cut(raw, []byte("\n"))
				return complete && json.Unmarshal(line, &reply) == nil && reply.Status != ""
			}); err != nil {
				raw, _ := os.ReadFile(stdout.Name())
				t.Fatalf("updater did not acknowledge or reject: %v %s", err, raw)
			}
			waitA := func() {
				t.Helper()
				writeHandoffProcessFile(t, marker("allow-A-exit"), nil)
				select {
				case err := <-sourceExit:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if !scenario.wantHandoff {
				if reply.Status != "check_failed" || reply.Reason == "" {
					t.Fatalf("source at %q got permission with registry InstallLocation %q and UserProgramFiles %q: %+v", app, location, scenario.userPrograms, reply)
				}
				assertHandoffProcessIdle(t, root, base, false)
				assertHandoffInstallerNotStarted(t, base)
				waitA()
				return
			}
			if reply.Status != "handoff_accepted" || reply.TransactionID != record.TransactionID {
				t.Fatalf("unexpected command response: %+v", reply)
			}
			claimed, err := ReadNativeRecord(root)
			if err != nil || claimed == nil || claimed.HandoffActor == nil || claimed.WindowsInstallVerified {
				t.Fatalf("acknowledgment did not name a durable unverified worker: %+v %v", claimed, err)
			}
			assertHandoffWorkerLocation(t, root, *claimed.HandoffActor)
			observation := time.Now().Add(time.Second)
			if err := pollHandoffProcess(ctx, func() bool {
				if alive, err := SameProcess(actor); err != nil || !alive {
					t.Fatalf("source exited before it was released: %v, %v", alive, err)
				}
				assertHandoffProcessIdle(t, root, base, true)
				assertHandoffInstallerNotStarted(t, base)
				return time.Now().After(observation)
			}); err != nil {
				t.Fatal(err)
			}
			waitA()
			if err := pollHandoffProcess(ctx, func() bool { _, err := os.Stat(marker("B-files-written")); return err == nil }); err != nil {
				t.Fatalf("installer did not replace the files after A quit: %v", err)
			}
			nsisPID, err := os.ReadFile(marker("NSIS-started"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(nsisPID))
			if err != nil {
				t.Fatal(err)
			}
			nsis, err := IdentifyProcess(pid)
			if err != nil {
				t.Fatalf("installer did not stay alive after writing B: %v", err)
			}
			var created, exited, kernel, user windows.Filetime
			if err := windows.GetProcessTimes(sourceHandle, &created, &exited, &kernel, &user); err != nil {
				t.Fatalf("read source exit time: %v", err)
			}
			sourceExitID := uint64(exited.HighDateTime)<<32 | uint64(exited.LowDateTime)
			if sourceExitID == 0 || nsis.StartID < sourceExitID {
				t.Fatalf("NSIS process began before A had exited: installer=%d, source exit=%d", nsis.StartID, sourceExitID)
			}
			observation = time.Now().Add(time.Second)
			if err := pollHandoffProcess(ctx, func() bool {
				assertHandoffProcessIdle(t, root, base, true)
				return time.Now().After(observation)
			}); err != nil {
				t.Fatal(err)
			}
			writeHandoffProcessFile(t, marker("allow-NSIS-exit"), nil)
			if err := pollHandoffProcess(ctx, func() bool {
				launches, err := os.ReadFile(marker("B-launches"))
				return err == nil && len(launches) != 0
			}); err != nil {
				t.Fatalf("verified install never launched B: %v", err)
			}
			if alive, err := SameProcess(nsis); err != nil || alive {
				t.Fatalf("B launched while installer was active: %v, %v", alive, err)
			}
			if verified, err := ReadNativeRecord(root); err != nil || verified == nil || !verified.WindowsInstallVerified || verified.Phase != "installing" {
				t.Fatalf("public launch lacked durable proof: %+v %v", verified, err)
			}
			if err := pollHandoffProcess(ctx, func() bool {
				alive, err := SameProcess(*claimed.HandoffActor)
				return err == nil && !alive
			}); err != nil {
				t.Fatalf("worker did not finish the handoff: %v", err)
			}
			if launches, err := os.ReadFile(marker("B-launches")); err != nil || string(launches) != "launched\n" {
				t.Fatalf("public launcher ran more than once: %q, %v", launches, err)
			}
		})
	}
}

func assertHandoffWorkerLocation(t *testing.T, root string, actor ProcessIdentity) {
	t.Helper()
	if alive, err := SameProcess(actor); !alive || err != nil {
		t.Fatalf("persisted worker is not live: %v, %v", alive, err)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(actor.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	length := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length); err != nil {
		t.Fatal(err)
	}
	worker := windows.UTF16ToString(buffer[:length])
	if !within(root, worker) {
		t.Fatalf("worker %q is outside the updater root %q", worker, root)
	}
}

func assertHandoffProcessIdle(t *testing.T, root, base string, claimed bool) {
	t.Helper()
	record, err := ReadNativeRecord(root)
	if err != nil || record == nil || record.WindowsInstallVerified || (record.HandoffActor != nil) != claimed {
		t.Fatalf("unexpected handoff or proof: %+v %v", record, err)
	}
	if _, err := os.Stat(filepath.Join(base, "B-launches")); !os.IsNotExist(err) {
		t.Fatalf("B ran before permission: %v", err)
	}
}

func assertHandoffInstallerNotStarted(t *testing.T, base string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(base, "NSIS-started")); !os.IsNotExist(err) {
		t.Fatalf("installer started before it received permission: %v", err)
	}
}
