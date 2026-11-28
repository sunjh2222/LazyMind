//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type windowsPolicyHandoffFixture struct {
	proof       windowsPolicyInstallProofFixture
	source      ProcessIdentity
	worker      ProcessIdentity
	packagePath string
	asarPath    string
	bAsar       []byte
	calls       []string
}

func newWindowsPolicyHandoffFixture(t *testing.T) *windowsPolicyHandoffFixture {
	t.Helper()
	f := &windowsPolicyHandoffFixture{
		proof:  newWindowsPolicyInstallProofFixture(t, "installing"),
		source: ProcessIdentity{PID: 171, StartID: 101},
		worker: ProcessIdentity{PID: 172, StartID: 202},
	}
	record := f.record(t)
	packageBytes, err := os.ReadFile(record.PackagePath)
	if err != nil {
		t.Fatal(err)
	}
	f.packagePath = filepath.Join(DownloadCacheDirectory(f.proof.native.root, f.proof.policy.config), record.PackageSHA256+".exe")
	if err := os.WriteFile(f.packagePath, packageBytes, 0600); err != nil {
		t.Fatal(err)
	}
	record.PackagePath = f.packagePath
	// The CLI obtains the source and worker identities from Windows. At the
	// supervisor boundary they are already identified, separate process actors.
	record.SourceActor = &f.source
	if err := writeNativeRecord(f.proof.native.root, record); err != nil {
		t.Fatal(err)
	}
	// The 3B1 fixture defines B's known, signed bytes. Keep B incomplete until
	// the process mock models NSIS finishing, so early proof cannot pass.
	f.asarPath = filepath.Join(f.proof.disk.root, "resources", "app.asar")
	f.bAsar, err = os.ReadFile(f.asarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.asarPath); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *windowsPolicyHandoffFixture) record(t *testing.T) *NativeRecord {
	t.Helper()
	record, err := ReadNativeRecord(f.proof.native.root)
	if err != nil || record == nil || record.TransactionID != f.proof.native.tx {
		t.Fatalf("could not read active handoff %s: %v", f.proof.native.tx, err)
	}
	return record
}

func (f *windowsPolicyHandoffFixture) assertClaim(t *testing.T, worker ProcessIdentity, verified bool) {
	t.Helper()
	record := f.record(t)
	if record.HandoffActor == nil || *record.HandoffActor != worker ||
		record.WindowsInstallVerified != verified || record.Phase != "installing" {
		t.Errorf("handoff actor=%v proof=%t phase=%s, want actor=%v proof=%t while installing",
			record.HandoffActor, record.WindowsInstallVerified, record.Phase, worker, verified)
	}
}

func (f *windowsPolicyHandoffFixture) processes(t *testing.T) windowsPolicyHandoffProcesses {
	return f.processesFor(t, f.worker, func(action string) { f.calls = append(f.calls, action) })
}

func (f *windowsPolicyHandoffFixture) processesFor(t *testing.T, worker ProcessIdentity, note func(string)) windowsPolicyHandoffProcesses {
	t.Helper()
	return windowsPolicyHandoffProcesses{
		ready: func() error {
			note("ready")
			f.assertClaim(t, worker, false)
			return nil
		},
		waitForSourceExit: func(_ context.Context, actor ProcessIdentity) error {
			note("wait for A")
			if actor != f.source {
				t.Errorf("source wait actor = %v, want %v", actor, f.source)
			}
			f.assertClaim(t, worker, false)
			return nil
		},
		install: func(_ context.Context, packagePath string, args []string) error {
			note("wait for NSIS")
			wantArgs := []string{"--updated", "/S", "--simple-install", "/currentuser"}
			if packagePath != f.packagePath || !slices.Equal(args, wantArgs) {
				t.Errorf("installer = %q %q, want %q %q", packagePath, args, f.packagePath, wantArgs)
			}
			f.assertClaim(t, worker, false)
			return os.WriteFile(f.asarPath, f.bAsar, 0600)
		},
		launch: func(executable string) error {
			note("launch B")
			if expected := filepath.Join(f.proof.disk.root, "LazyMind.exe"); executable != expected {
				t.Errorf("B executable = %q, want public launcher %q", executable, expected)
			}
			f.assertClaim(t, worker, true)
			return nil
		},
	}
}

func (f *windowsPolicyHandoffFixture) run(ctx context.Context, tx string, source, worker ProcessIdentity, processes windowsPolicyHandoffProcesses) error {
	return runWindowsPolicyHandoff(ctx, f.proof.native.root, f.proof.policy.config, tx, source, worker,
		processes, func() time.Time { return policyTime })
}

func (f *windowsPolicyHandoffFixture) assertCalls(t *testing.T, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Errorf("handoff calls = %v, want %v", f.calls, want)
	}
}

func TestWindowsPolicyHandoffStartsPublicLauncherOnlyAfterProof(t *testing.T) {
	f := newWindowsPolicyHandoffFixture(t)
	scope := UpdateScope{Installation: f.record(t).InstallationRoot}
	if err := CheckStartup(t.Context(), f.proof.native.root, scope); ErrorCode(err) != "update_in_progress" {
		t.Fatalf("public launcher admitted before the handoff: %v", err)
	}
	if err := f.run(t.Context(), f.proof.native.tx, f.source, f.worker, f.processes(t)); err != nil {
		t.Fatalf("handoff of exact signed target failed: %v", err)
	}
	f.assertCalls(t, "ready", "wait for A", "wait for NSIS", "launch B")
	f.assertClaim(t, f.worker, true)
	if err := CheckStartup(t.Context(), f.proof.native.root, scope); err != nil {
		t.Fatalf("verified public launcher was blocked: %v", err)
	}
}

func TestWindowsPolicyHandoffCannotBeClaimedTwice(t *testing.T) {
	// This module defaults to legacy timer channels, which synctest cannot virtualize.
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",asynctimerchan=0")
	synctest.Test(t, func(t *testing.T) {
		f := newWindowsPolicyHandoffFixture(t)
		lock, err := nativeStateLock(t.Context(), f.proof.native.root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if lock != nil {
				releaseLock(lock)
			}
		}()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		type event struct {
			worker ProcessIdentity
			action string
			err    error
		}
		start := make(chan struct{})
		entered := make(chan ProcessIdentity, 2)
		sourceExited := make(chan struct{})
		events := make(chan event, 10)
		next := func() event {
			select {
			case value := <-events:
				return value
			case <-ctx.Done():
				t.Fatal("timed out waiting for handoff event")
				return event{}
			}
		}
		for _, worker := range []ProcessIdentity{f.worker, {PID: 173, StartID: 303}} {
			go func(worker ProcessIdentity) {
				<-start
				processes := f.processesFor(t, worker, func(action string) { events <- event{worker: worker, action: action} })
				wait := processes.waitForSourceExit
				processes.waitForSourceExit = func(ctx context.Context, source ProcessIdentity) error {
					if err := wait(ctx, source); err != nil {
						return err
					}
					select {
					case <-sourceExited:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				entered <- worker
				err := f.run(ctx, f.proof.native.tx, f.source, worker, processes)
				events <- event{worker: worker, action: "done", err: err}
			}(worker)
		}
		close(start)
		<-entered
		<-entered
		synctest.Wait()
		select {
		case value := <-events:
			t.Fatalf("worker %v reported %q before native state lock release: %v", value.worker, value.action, value.err)
		default:
		}
		if record := f.record(t); record.HandoffActor != nil {
			t.Fatalf("worker claimed the handoff before native state lock release: %v", record.HandoffActor)
		}
		releaseLock(lock)
		lock = nil

		var ready, waiting, rejected ProcessIdentity
		for i := 0; i < 3; i++ {
			value := next()
			switch value.action {
			case "ready":
				if ready.PID != 0 {
					t.Fatalf("both workers acknowledged the same handoff: %v and %v", ready, value.worker)
				}
				ready = value.worker
			case "wait for A":
				if waiting.PID != 0 {
					t.Fatalf("both workers advanced to waiting for A: %v and %v", waiting, value.worker)
				}
				waiting = value.worker
			case "done":
				if value.err == nil || rejected.PID != 0 {
					t.Fatalf("unexpected result while A was still running: %+v", value)
				}
				rejected = value.worker
			default:
				t.Fatalf("worker advanced beyond waiting for A: %+v", value)
			}
		}
		if ready.PID == 0 || ready != waiting || rejected.PID == 0 || rejected == ready {
			t.Fatalf("expected one worker waiting for A and the other rejected: ready=%v waiting=%v rejected=%v", ready, waiting, rejected)
		}
		f.assertClaim(t, ready, false)
		close(sourceExited)
		for _, action := range []string{"wait for NSIS", "launch B", "done"} {
			value := next()
			if value.worker != ready || value.action != action || value.err != nil {
				t.Fatalf("handoff event = %+v, want %s by %v", value, action, ready)
			}
		}
		synctest.Wait()
		if len(events) != 0 {
			t.Errorf("unexpected additional handoff event: %+v", <-events)
		}
		retry := f.processesFor(t, ready, func(action string) { t.Errorf("repeat handoff reached %q", action) })
		if err := f.run(ctx, f.proof.native.tx, f.source, ready, retry); err == nil {
			t.Error("the recorded worker was allowed to repeat the handoff")
		}
		f.assertClaim(t, ready, true)
	})
}

func TestWindowsPolicyHandoffRejectsWrongTransactionSourceOrPackage(t *testing.T) {
	for _, scenario := range []string{"wrong transaction", "reused source PID", "changed package"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWindowsPolicyHandoffFixture(t)
			source, tx, reason := f.source, f.proof.native.tx, "update_transaction_conflict"
			switch scenario {
			case "wrong transaction":
				tx = strings.Repeat("e", 32)
			case "reused source PID":
				source.StartID++
			case "changed package":
				swapHandoffPackage(t, f.packagePath)
				reason = "artifact_hash"
			}
			if err := f.run(t.Context(), tx, source, f.worker, f.processes(t)); ErrorCode(err) != reason {
				t.Errorf("handoff returned %v, want %s", err, reason)
			}
			f.assertCalls(t)
			if record := f.record(t); record.HandoffActor != nil || record.WindowsInstallVerified {
				t.Errorf("rejected handoff claimed the transaction: actor=%v proof=%t", record.HandoffActor, record.WindowsInstallVerified)
			}
		})
	}
}

func (f *windowsPolicyHandoffFixture) acceptChangedPackagePolicy(t *testing.T) {
	t.Helper()
	target := f.proof.policy.policy["releases"].([]any)[1].(map[string]any)
	target["package"].(map[string]any)["sha256"] = strings.Repeat("e", 64)
	f.proof.policy.policy["policySequence"] = 43
	envelope := f.proof.policy.envelope(t)
	decision, err := Check(t.Context(), envelope, f.proof.policy.config,
		f.proof.policy.state, f.proof.native.root, false, policyTime)
	if err != nil || decision.Next == nil || decision.Next.ID != "r2" || decision.PolicySequence != 43 {
		t.Fatalf("could not accept the changed signed target: %+v, %v", decision, err)
	}
	accepted, err := ReadAcceptedEnvelope(f.proof.policy.config, f.proof.native.root)
	if err != nil || !bytes.Equal(accepted, envelope) {
		t.Fatalf("active handoff ledger did not retain the changed signed target: %v", err)
	}
}

func TestWindowsPolicyHandoffRejectsPackageWithdrawnByLaterSignedPolicy(t *testing.T) {
	f := newWindowsPolicyHandoffFixture(t)
	f.acceptChangedPackagePolicy(t)
	err := f.run(t.Context(), f.proof.native.tx, f.source, f.worker, f.processes(t))
	if ErrorCode(err) != "offer_changed" {
		t.Errorf("handoff returned %v, want offer_changed", err)
	}
	f.assertCalls(t)
	if record := f.record(t); record.HandoffActor != nil || record.WindowsInstallVerified {
		t.Errorf("changed signed target claimed the transaction: actor=%v proof=%t", record.HandoffActor, record.WindowsInstallVerified)
	}
}

func swapHandoffPackage(t *testing.T, filename string) {
	t.Helper()
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPolicyHandoffDoesNotLaunchAfterWaitInstallOrProofFailure(t *testing.T) {
	for _, scenario := range []string{
		"source timeout", "package changed while waiting", "signed target changed while waiting", "installer failed", "B proof failed",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newWindowsPolicyHandoffFixture(t)
			processes := f.processes(t)
			wait, install := processes.waitForSourceExit, processes.install
			processes.waitForSourceExit = func(ctx context.Context, actor ProcessIdentity) error {
				if err := wait(ctx, actor); err != nil {
					return err
				}
				switch scenario {
				case "source timeout":
					return context.DeadlineExceeded
				case "package changed while waiting":
					swapHandoffPackage(t, f.packagePath)
				case "signed target changed while waiting":
					f.acceptChangedPackagePolicy(t)
				}
				return nil
			}
			processes.install = func(ctx context.Context, packagePath string, args []string) error {
				if err := install(ctx, packagePath, args); err != nil {
					return err
				}
				switch scenario {
				case "installer failed":
					// Even a complete set of B files must not override a failing installer exit.
					return errors.New("fixture installer returned nonzero status")
				case "B proof failed":
					// A successful installer exit does not make changed B bytes trustworthy.
					return os.Remove(f.asarPath)
				}
				return nil
			}
			err := f.run(t.Context(), f.proof.native.tx, f.source, f.worker, processes)
			if err == nil {
				t.Error("handoff unexpectedly succeeded")
			}
			switch scenario {
			case "source timeout", "package changed while waiting", "signed target changed while waiting":
				f.assertCalls(t, "ready", "wait for A")
			default:
				f.assertCalls(t, "ready", "wait for A", "wait for NSIS")
			}
			if scenario == "signed target changed while waiting" && ErrorCode(err) != "offer_changed" {
				t.Errorf("handoff returned %v, want offer_changed", err)
			}
			if scenario == "B proof failed" && ErrorCode(err) != "installed_manifest_file" {
				t.Errorf("B verification returned %v, want installed_manifest_file", err)
			}
			record := f.record(t)
			switch scenario {
			case "source timeout", "installer failed", "B proof failed":
				if strings.TrimSpace(record.Reason) == "" {
					t.Error("claimed handoff failure did not persist a reason in the transaction")
				}
			}
			if record.WindowsInstallVerified || record.Phase == "activated" {
				t.Errorf("failed handoff left proof=%t and phase=%s", record.WindowsInstallVerified, record.Phase)
			}
			scope := UpdateScope{Installation: record.InstallationRoot}
			if err := CheckStartup(t.Context(), f.proof.native.root, scope); ErrorCode(err) != "update_in_progress" {
				t.Errorf("public launcher admitted after a failed handoff: %v", err)
			}
		})
	}
}
