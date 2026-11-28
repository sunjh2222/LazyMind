//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"os"
	"testing"
)

func TestWindowsPolicyInstallStartGateBlocksUnverifiedHandoff(t *testing.T) {
	f := newWindowsPolicyNativeManifestRecordFixture(t)
	sameInstallation := UpdateScope{Installation: f.installation}
	foreignInstallation := UpdateScope{Installation: t.TempDir()}
	// The public launcher checks its installation path before it starts Electron.
	f.create(t)
	if err := CheckStartup(t.Context(), f.root, sameInstallation); err != nil {
		t.Fatalf("pending transaction blocked the source application's normal startup: %v", err)
	}
	f.command(t, "installing", map[string]any{
		"transactionId": f.tx, "dataProfile": f.record["sourceDataProfile"],
		"componentMode": "absent", "sourcePaused": true, "sourceActive": 0,
	}, "installing", "")
	t.Run("installing", func(t *testing.T) {
		if err := CheckStartup(t.Context(), f.root, sameInstallation); ErrorCode(err) != "update_in_progress" {
			t.Errorf("unverified installation's normal launcher was not blocked: %v", err)
		}
		if err := CheckStartup(t.Context(), f.root, foreignInstallation); err != nil {
			t.Errorf("unrelated installation was blocked: %v", err)
		}
	})
	f.command(t, "repair", map[string]any{"transactionId": f.tx, "reason": "native_handoff_unknown"}, "repair", "")
	t.Run("repair", func(t *testing.T) {
		if err := CheckStartup(t.Context(), f.root, sameInstallation); ErrorCode(err) != "update_in_progress" {
			t.Errorf("unverified installation in repair was not blocked: %v", err)
		}
		if err := CheckStartup(t.Context(), f.root, foreignInstallation); err != nil {
			t.Errorf("unrelated installation was blocked during repair: %v", err)
		}
	})
}

func TestWindowsPolicyInstallStartGateLeavesLegacyWindowsAdmitted(t *testing.T) {
	f := newNativeStateFixture(t)
	f.record["platform"], f.record["arch"] = "windows", "amd64"
	f.installing(t)
	if err := CheckStartup(t.Context(), f.root, UpdateScope{Installation: f.installation}); err != nil {
		t.Fatalf("legacy Windows launcher admission changed: %v", err)
	}
}

func TestWindowsPolicyInstallStartGateRejectsActivationWithoutProof(t *testing.T) {
	f := newWindowsPolicyNativeManifestRecordFixture(t)
	f.installing(t)
	before, err := os.ReadFile(f.filename)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := CurrentProcessIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f.command(t, "activated", map[string]any{
		"transactionId": f.tx, "releaseId": f.record["targetReleaseId"], "dataProfile": f.record["targetDataProfile"],
		"componentMode": "absent", "components": map[string]string{}, "owner": "fixture-target-owner", "actor": actor,
	}, "blocked", "update_incomplete")
	f.command(t, "activated", map[string]any{
		"transactionId": f.tx, "releaseId": f.record["targetReleaseId"], "dataProfile": f.record["targetDataProfile"],
		"componentMode": "absent", "components": map[string]string{}, "owner": "fixture-target-owner", "actor": actor,
		"windowsInstallVerified": true,
	}, "blocked", "update_request_invalid")
	after, err := os.ReadFile(f.filename)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("unverified activation changed the installing transaction: %v", err)
	}
}

func TestWindowsPolicyInstallStartGateRejectsAssertedProofAtCreate(t *testing.T) {
	f := newWindowsPolicyNativeManifestRecordFixture(t)
	f.record["windowsInstallVerified"] = true
	f.command(t, "create", f.record, "blocked", "update_request_invalid")
	if _, err := os.Lstat(f.filename); !os.IsNotExist(err) {
		t.Fatalf("a caller-authored proof created a live transaction: %v", err)
	}
}
