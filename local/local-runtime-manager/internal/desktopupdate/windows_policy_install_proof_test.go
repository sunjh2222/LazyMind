//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type windowsPolicyInstallProofFixture struct {
	native *nativeStateFixture
	policy *policyFixture
	disk   *installedManifestFixture
}

func newWindowsPolicyInstallProofFixture(t *testing.T, phase string) windowsPolicyInstallProofFixture {
	t.Helper()
	native := newNativeStateFixture(t)
	policy := newWindowsPolicyManifestFixture(t)
	disk := newInstalledManifestFixture(t)
	disk.policy = policy
	disk.manifest["trustDomain"] = policy.config.TrustDomain
	disk.manifest["releaseId"] = "r2"

	const catalogPath = "resources/runtime/config/python-components.json"
	catalog := []byte(`{"schemaVersion":1,"platform":"windows","arch":"amd64","components":{}}`)
	disk.writeFile(t, catalogPath, catalog)
	files := disk.manifest["files"].([]map[string]any)
	for i, entry := range files {
		if entry["path"] == catalogPath {
			files[i] = fileManifestEntry(catalogPath, catalog)
		}
	}
	manifest := disk.envelope(t, policy.private)
	disk.writeFile(t, "resources/installed-manifest.json", manifest)
	cache := DownloadCacheDirectory(native.root, policy.config)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	manifestEntry := fileManifestEntry("installed-manifest.json", manifest)
	manifestPath := filepath.Join(cache, manifestEntry["sha256"].(string)+".json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	catalogEntry := fileManifestEntry(catalogPath, catalog)
	catalogCache := filepath.Join(cache, catalogEntry["sha256"].(string)+".json")
	if err := os.WriteFile(catalogCache, catalog, 0600); err != nil {
		t.Fatal(err)
	}
	target := policy.policy["releases"].([]any)[1].(map[string]any)
	target["installedManifest"] = Artifact{
		URL: windowsPolicyManifestArtifact("r2").URL, SHA256: manifestEntry["sha256"].(string), Size: int64(len(manifest)),
	}
	components := target["components"].(map[string]any)
	components["sha256"], components["size"] = catalogEntry["sha256"], len(catalog)
	edge := policy.policy["edges"].([]any)[0].(map[string]any)
	edge["componentTransitions"].(map[string]any)["rag"].(map[string]any)["targetComponentsSHA256"] = components["sha256"]
	archive := target["package"].(map[string]any)
	archive["sha256"], archive["size"] = native.record["packageSHA256"], native.record["packageSize"]
	archive["unpackedBytes"] = 4096
	decision, err := Check(t.Context(), policy.envelope(t), policy.config, policy.state, native.root, false, policyTime)
	if err != nil || decision.Status != "direct" || decision.Next == nil || decision.Next.ID != "r2" {
		t.Fatalf("could not accept signed target: %+v, %v", decision, err)
	}

	installation := filepath.Join(disk.root, "resources", "runtime")
	native.record["installationRoot"] = installation
	native.record["scope"].(map[string]any)["installation"] = installation
	native.record["mode"], native.record["trustDomain"] = policy.config.Mode, policy.config.TrustDomain
	native.record["platform"], native.record["arch"] = policy.config.Platform, policy.config.Arch
	native.record["signingIdentity"] = policy.config.SigningIdentity
	native.record["sourceReleaseId"], native.record["targetReleaseId"] = decision.Source.ID, decision.Next.ID
	native.record["policyDigest"], native.record["policySequence"] = decision.PolicyDigest, decision.PolicySequence
	native.record["packageUnpackedBytes"] = decision.Next.Package.UnpackedBytes
	native.record["componentsPath"], native.record["componentsSHA256"], native.record["componentsSize"] = catalogCache, components["sha256"], len(catalog)
	native.record["installedManifestPath"], native.record["installedManifestSHA256"], native.record["installedManifestSize"] =
		manifestPath, decision.Next.InstalledManifest.SHA256, decision.Next.InstalledManifest.Size
	native.create(t)
	if phase != "pending" {
		native.command(t, "installing", map[string]any{
			"transactionId": native.tx, "dataProfile": native.record["sourceDataProfile"],
			"componentMode": "absent", "sourcePaused": true, "sourceActive": 0,
		}, "installing", "")
	}
	if phase == "repair" {
		native.command(t, "repair", map[string]any{"transactionId": native.tx, "reason": "native_handoff_unknown"}, "repair", "")
	}
	return windowsPolicyInstallProofFixture{native: native, policy: policy, disk: disk}
}

func readWindowsInstallProof(t *testing.T, f windowsPolicyInstallProofFixture) bool {
	t.Helper()
	record, err := ReadNativeRecord(f.native.root)
	if err != nil || record == nil || record.TransactionID != f.native.tx || record.Phase != "installing" {
		t.Fatalf("installation verification changed the pending handoff: record=%+v err=%v", record, err)
	}
	var proof struct {
		Verified bool `json:"windowsInstallVerified"`
	}
	if err := json.Unmarshal(nativeJSON(t, record), &proof); err != nil {
		t.Fatal(err)
	}
	return proof.Verified
}

func (f windowsPolicyInstallProofFixture) verify(ctx context.Context, tx string) error {
	return VerifyAndRecordWindowsPolicyInstallation(ctx, f.native.root, f.policy.config, tx, policyTime)
}

func TestWindowsPolicyInstallProofRequiresVerifiedInstalledFiles(t *testing.T) {
	f := newWindowsPolicyInstallProofFixture(t, "installing")
	if err := f.verify(t.Context(), f.native.tx); err != nil {
		t.Fatalf("exact installed target did not produce proof: %v", err)
	}
	if !readWindowsInstallProof(t, f) {
		t.Fatal("verified installed target did not persist startup proof")
	}
	scope := UpdateScope{Installation: f.native.record["installationRoot"].(string)}
	if err := CheckStartup(t.Context(), f.native.root, scope); err != nil {
		t.Fatalf("launcher still blocks the verified installation: %v", err)
	}
	actor, err := CurrentProcessIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f.native.command(t, "activated", map[string]any{
		"transactionId": f.native.tx, "releaseId": "r2", "dataProfile": f.native.record["targetDataProfile"],
		"componentMode": "absent", "components": map[string]string{}, "owner": "fixture-target-owner", "actor": actor,
	}, "activated", "")
}

func TestWindowsPolicyInstallProofRequiresInstallingTransaction(t *testing.T) {
	for _, phase := range []string{"pending", "repair"} {
		t.Run(phase, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, phase)
			before, err := os.ReadFile(f.native.filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.verify(t.Context(), f.native.tx); err == nil {
				t.Fatalf("valid files obtained proof in the %s phase", phase)
			}
			after, err := os.ReadFile(f.native.filename)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("verification changed the %s transaction: %v", phase, err)
			}
		})
	}
}

func TestWindowsPolicyInstallProofRejectsWrongTransactionOrInstalledFiles(t *testing.T) {
	for _, kind := range []string{"different transaction", "different source identity", "changed Electron file", "changed embedded manifest", "changed cached manifest"} {
		t.Run(kind, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "installing")
			tx := f.native.tx
			switch kind {
			case "different transaction":
				tx = "22222222222222222222222222222222"
			case "different source identity":
				f.policy.config.ReleaseID = "unrelated-source"
			case "changed Electron file":
				f.disk.writeFile(t, "resources/app.asar", []byte("INVALID!! fixture: resources/app.asar"))
			case "changed embedded manifest":
				f.disk.writeFile(t, "resources/installed-manifest.json", []byte("unexpected manifest"))
			case "changed cached manifest":
				path := f.native.record["installedManifestPath"].(string)
				cached, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				cached[0] ^= 1
				if err := os.WriteFile(path, cached, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.verify(t.Context(), tx); err == nil {
				t.Fatal("invalid installation obtained startup proof")
			}
			if readWindowsInstallProof(t, f) {
				t.Fatal("failed installation verification left startup proof")
			}
		})
	}
}

func TestWindowsPolicyInstallProofRejectsChangedSignedTargetArtifacts(t *testing.T) {
	for _, changed := range []string{"installedManifest", "package"} {
		t.Run(changed, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "installing")
			target := f.policy.policy["releases"].([]any)[1].(map[string]any)
			digest := fixtureArtifact("other-" + changed)["sha256"].(string)
			if changed == "installedManifest" {
				artifact := target[changed].(Artifact)
				artifact.SHA256 = digest
				target[changed] = artifact
			} else {
				target[changed].(map[string]any)["sha256"] = digest
			}
			f.policy.policy["policySequence"] = 43
			newEnvelope := f.policy.envelope(t)
			decision, err := Check(t.Context(), newEnvelope, f.policy.config, f.policy.state, f.native.root, false, policyTime)
			if err != nil || decision.Status != "direct" || decision.PolicySequence != 43 {
				t.Fatalf("accept separately signed policy fixture: %+v, %v", decision, err)
			}
			if accepted, err := ReadAcceptedEnvelope(f.policy.config, f.native.root); err != nil || !bytes.Equal(accepted, newEnvelope) {
				t.Fatalf("transaction ledger did not retain the changed policy: %v", err)
			}
			if err := f.verify(t.Context(), f.native.tx); err == nil {
				t.Fatal("installation proved a different target from the one bound to the native transaction")
			}
			if readWindowsInstallProof(t, f) {
				t.Fatal("changed signed target left startup proof")
			}
		})
	}
}
