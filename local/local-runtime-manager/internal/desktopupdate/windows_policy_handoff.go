package desktopupdate

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type windowsPolicyHandoffProcesses struct {
	ready             func() error
	waitForSourceExit func(context.Context, ProcessIdentity) error
	install           func(context.Context, string, []string) error
	launch            func(string) error
}

// RunWindowsPolicyHandoff replaces a verified installation using the OS
// process actions supplied by its staged source updater. The caller identifies
// both processes through the OS and waits for the installer to exit.
func RunWindowsPolicyHandoff(ctx context.Context, root string, c Config, tx string, source, worker ProcessIdentity,
	ready func() error,
	waitForSourceExit func(context.Context, ProcessIdentity) error,
	install func(context.Context, string, []string) error,
	launch func(string) error,
) error {
	return runWindowsPolicyHandoff(ctx, root, c, tx, source, worker,
		windowsPolicyHandoffProcesses{ready, waitForSourceExit, install, launch}, time.Now)
}

func runWindowsPolicyHandoff(ctx context.Context, root string, c Config, tx string, source, worker ProcessIdentity,
	processes windowsPolicyHandoffProcesses, now func() time.Time,
) (result error) {
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if c.Mode != "windows-policy" || processes.ready == nil || processes.waitForSourceExit == nil ||
		processes.install == nil || processes.launch == nil || now == nil {
		return failure("update_request_invalid", nil)
	}
	if source.PID <= 0 || worker.PID <= 0 || source.PID == worker.PID || source.StartID == 0 || worker.StartID == 0 {
		return failure("update_transaction_conflict", nil)
	}
	if err := ValidateWindowsPolicyDownloadCache(root, c); err != nil {
		return err
	}
	record, err := claimWindowsPolicyHandoff(ctx, root, c, tx, source, worker, now())
	if err != nil {
		return err
	}
	defer func() {
		if result != nil {
			// A timeout or cancellation after claiming the transaction must
			// still leave a diagnostic for the next startup.
			err := recordWindowsPolicyHandoffFailure(context.WithoutCancel(ctx), root, tx, worker, ErrorCode(result))
			if err != nil {
				result = errors.Join(result, err)
			}
		}
	}()

	if err := processes.ready(); err != nil {
		return failure("native_handoff_unknown", err)
	}
	if err := processes.waitForSourceExit(ctx, source); err != nil {
		return failure("native_handoff_unknown", err)
	}
	if err := recheckWindowsPolicyHandoff(ctx, root, c, record, now()); err != nil {
		return err
	}
	if err := processes.install(ctx, record.PackagePath, []string{"--updated", "/S", "--simple-install", "/currentuser"}); err != nil {
		return failure("native_install_failed", err)
	}
	if err := VerifyAndRecordWindowsPolicyInstallation(ctx, root, c, tx, now()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	appRoot := filepath.Dir(filepath.Dir(record.InstallationRoot))
	if err := processes.launch(filepath.Join(appRoot, "LazyMind.exe")); err != nil {
		return failure("update_startup_failed", err)
	}
	return nil
}

func claimWindowsPolicyHandoff(ctx context.Context, root string, c Config, tx string, source, worker ProcessIdentity, now time.Time) (*NativeRecord, error) {
	lock, err := nativeStateLock(ctx, root)
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	record, err := ReadNativeRecord(root)
	if err != nil {
		return nil, err
	}
	if record == nil || record.TransactionID != tx || record.Mode != c.Mode || record.Phase != "installing" ||
		record.RepairTarget != nil || record.HandoffActor != nil || record.WindowsInstallVerified ||
		record.SourceActor == nil || *record.SourceActor != source || record.SourceReleaseID != c.ReleaseID || record.SourceVersion != c.Version ||
		record.TrustDomain != c.TrustDomain || record.Audience != c.Audience || record.Channel != c.Channel ||
		record.SigningIdentity != c.SigningIdentity || record.Platform != c.Platform || record.Arch != c.Arch {
		return nil, failure("update_transaction_conflict", nil)
	}
	if err := checkWindowsPolicyHandoffArtifacts(ctx, root, c, record, now); err != nil {
		return nil, err
	}
	record.HandoffActor = &worker
	if err := writeNativeRecord(root, record); err != nil {
		return nil, failure("update_record_write", err)
	}
	return record, nil
}

func recheckWindowsPolicyHandoff(ctx context.Context, root string, c Config, expected *NativeRecord, now time.Time) error {
	lock, err := nativeStateLock(ctx, root)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	record, err := ReadNativeRecord(root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(record, expected) {
		return failure("update_transaction_conflict", nil)
	}
	return checkWindowsPolicyHandoffArtifacts(ctx, root, c, record, now)
}

// StartWindowsPolicyInstaller locks the claimed intent and current signed
// policy through process creation. The caller pins the verified installer and
// waits for the child outside both locks.
func StartWindowsPolicyInstaller(ctx context.Context, root string, c Config, expected NativeRecord, worker ProcessIdentity, start func() error) error {
	stateLock, err := nativeStateLock(ctx, root)
	if err != nil {
		return err
	}
	defer releaseLock(stateLock)
	record, err := ReadNativeRecord(root)
	if err != nil {
		return err
	}
	expected.HandoffActor = &worker
	if expected.Mode != "windows-policy" || expected.Phase != "installing" || expected.WindowsInstallVerified || !reflect.DeepEqual(&expected, record) {
		return failure("update_transaction_conflict", nil)
	}
	ledgerLock, err := lockPolicyLedger(ctx, policyLedgerDirectory(root, c))
	if err != nil {
		return err
	}
	defer releaseLock(ledgerLock)
	envelope, err := ReadAcceptedEnvelope(c, root)
	if err != nil {
		return err
	}
	if _, err := windowsInstallProofTarget(envelope, c, record, time.Now()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return start()
}

// MarkWindowsPolicyHandoffInterrupted records a dispatcher failure only if
// the failed child was the worker that successfully took this transaction.
func MarkWindowsPolicyHandoffInterrupted(ctx context.Context, root, tx string, worker ProcessIdentity, reason string) error {
	return recordWindowsPolicyHandoffFailure(ctx, root, tx, worker, reason)
}

// MarkWindowsPolicyHandoffSourceRejected records a failure before a worker
// takes the transaction. A repeat cannot replace an existing repair reason.
func MarkWindowsPolicyHandoffSourceRejected(ctx context.Context, root, tx string, source ProcessIdentity, reason string) error {
	lock, err := nativeStateLock(ctx, root)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	record, err := ReadNativeRecord(root)
	if err != nil {
		return err
	}
	if record == nil || record.TransactionID != tx || record.Mode != "windows-policy" ||
		record.SourceActor == nil || *record.SourceActor != source {
		return failure("update_transaction_conflict", nil)
	}
	if record.Phase == "repair" {
		return nil
	}
	if record.Phase != "installing" || record.HandoffActor != nil || record.WindowsInstallVerified {
		return failure("update_transaction_conflict", nil)
	}
	return markWindowsPolicyHandoffRepair(root, record, reason)
}

func checkWindowsPolicyHandoffArtifacts(ctx context.Context, root string, c Config, record *NativeRecord, now time.Time) error {
	envelope, err := ReadAcceptedEnvelope(c, root)
	if err != nil {
		return err
	}
	target, err := windowsInstallProofTarget(envelope, c, record, now)
	if err != nil {
		return err
	}
	if err := ValidateWindowsPolicyDownloadCache(root, c); err != nil {
		return err
	}
	cache := DownloadCacheDirectory(root, c)
	for _, file := range []struct {
		path, extension string
		artifact        Artifact
	}{
		{record.PackagePath, ".exe", target.Package},
		{record.ComponentsPath, ".json", target.Components},
		{record.InstalledManifestPath, ".json", *target.InstalledManifest},
	} {
		if file.path != filepath.Join(cache, file.artifact.SHA256+file.extension) {
			return failure("path_invalid", nil)
		}
		if err := VerifyArtifact(ctx, file.path, file.artifact); err != nil {
			return err
		}
	}
	current, err := ReadAcceptedEnvelope(c, root)
	if err != nil || !bytes.Equal(current, envelope) {
		return failure("offer_changed", err)
	}
	return ctx.Err()
}

func recordWindowsPolicyHandoffFailure(ctx context.Context, root, tx string, worker ProcessIdentity, reason string) error {
	lock, err := nativeStateLock(ctx, root)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	record, err := ReadNativeRecord(root)
	if err != nil {
		return err
	}
	if record == nil || record.TransactionID != tx || record.Mode != "windows-policy" || record.Phase != "installing" ||
		record.HandoffActor == nil || *record.HandoffActor != worker {
		return nil
	}
	return markWindowsPolicyHandoffRepair(root, record, reason)
}

func markWindowsPolicyHandoffRepair(root string, record *NativeRecord, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return failure("update_request_invalid", nil)
	}
	record.Phase = "repair"
	record.WindowsInstallVerified = false
	record.Reason = reason
	if err := validateNativeRecord(record); err != nil {
		return err
	}
	if err := writeNativeRecord(root, record); err != nil {
		return failure("update_record_write", err)
	}
	return nil
}
