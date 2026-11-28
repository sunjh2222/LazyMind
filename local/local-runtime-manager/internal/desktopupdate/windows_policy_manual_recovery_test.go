//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func makeWindowsRecoveryRecord(t *testing.T, f windowsPolicyInstallProofFixture) *NativeRecord {
	t.Helper()
	r, err := ReadNativeRecord(f.native.root)
	if err != nil || r == nil {
		t.Fatalf("read failed transaction: %+v, %v", r, err)
	}
	current, err := CurrentProcessIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// Both previous identities belong to an earlier boot. Use a reused PID
	// with different creation stamps to exercise the actual process identity check.
	r.SourceActor = &ProcessIdentity{PID: current.PID, StartID: current.StartID + 1}
	r.HandoffActor = &ProcessIdentity{PID: current.PID, StartID: current.StartID + 2}
	r.BootID = strings.Repeat("a", 64)
	if err := writeNativeRecord(f.native.root, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func windowsRecoveryCatalog(t *testing.T, f windowsPolicyInstallProofFixture, release map[string]any, target ComponentDescriptor) Artifact {
	t.Helper()
	const catalogPath = "resources/runtime/config/python-components.json"
	catalog := nativeJSON(t, map[string]any{"schemaVersion": 1, "platform": "windows", "arch": "amd64",
		"components": map[string]ComponentDescriptor{"rag": target}})
	f.disk.writeFile(t, catalogPath, catalog)
	entries := f.disk.manifest["files"].([]map[string]any)
	for i, entry := range entries {
		if entry["path"] == catalogPath {
			entries[i] = fileManifestEntry(catalogPath, catalog)
		}
	}
	component := release["components"].(map[string]any)
	component["sha256"] = fileManifestEntry(catalogPath, catalog)["sha256"].(string)
	component["size"] = int64(len(catalog))
	return Artifact{SHA256: component["sha256"].(string), Size: int64(len(catalog))}
}

func addWindowsRecoverySelection(t *testing.T, f windowsPolicyInstallProofFixture, r *NativeRecord) {
	t.Helper()
	source := ComponentDescriptor{ID: "rag", Revision: strings.Repeat("c", 64), SHA256: strings.Repeat("d", 64),
		Tag: strings.Repeat("a", 64), BaseFingerprint: strings.Repeat("b", 64), PythonABI: "cp311",
		Platform: "windows", Arch: "amd64", Packages: map[string]string{"fixture-package": "1"},
		SizeBytes: 12, UnpackedBytes: 20}
	target := source
	target.Revision = strings.Repeat("e", 64)
	r.WantedComponents, r.SourceHadRAG, r.ComponentMode = []string{"rag"}, true, "sync-target"
	r.ComponentUpdates = map[string]NativeComponentUpdate{"rag": {Source: source, Target: target}}
	f.policy.state.Components = map[string]string{"rag": source.Revision}
	f.policy.state.ComponentMetadata = map[string]ComponentDescriptor{"rag": source}
	release := f.policy.policy["releases"].([]any)[1].(map[string]any)
	catalog := windowsRecoveryCatalog(t, f, release, target)
	r.ComponentsSHA256, r.ComponentsSize = catalog.SHA256, catalog.Size
	edge := f.policy.policy["edges"].([]any)[0].(map[string]any)
	edge["componentTransitions"].(map[string]any)["rag"].(map[string]any)["targetComponentsSHA256"] = catalog.SHA256
	manifest := f.disk.envelope(t, f.policy.private)
	f.disk.writeFile(t, "resources/installed-manifest.json", manifest)
	artifact := windowsPolicyManifestArtifact(r.TargetReleaseID)
	artifact.SHA256 = fileManifestEntry("installed-manifest.json", manifest)["sha256"].(string)
	artifact.Size = int64(len(manifest))
	release["installedManifest"] = artifact
	r.InstalledManifestSHA256, r.InstalledManifestSize = artifact.SHA256, artifact.Size
	cache := DownloadCacheDirectory(f.native.root, f.policy.config)
	r.InstalledManifestPath = filepath.Join(cache, artifact.SHA256+".json")
	r.ComponentsPath = filepath.Join(cache, catalog.SHA256+".json")
	f.policy.policy["policySequence"] = r.PolicySequence + 1
	decision, err := Check(t.Context(), f.policy.envelope(t), f.policy.config, f.policy.state, f.native.root, false, policyTime)
	if err != nil || decision.Status != "direct" || decision.Next == nil || decision.Next.ID != r.TargetReleaseID {
		t.Fatalf("authorize selected RAG and original target: %+v, %v", decision, err)
	}
	r.PolicyDigest, r.PolicySequence = decision.PolicyDigest, decision.PolicySequence
	if err := writeNativeRecord(f.native.root, r); err != nil {
		t.Fatal(err)
	}
}

func windowsRecoveryForwardBundle(t *testing.T, f windowsPolicyInstallProofFixture, r *NativeRecord, approved bool) Config {
	t.Helper()
	config := f.policy.config
	forward := f.policy.policy["releases"].([]any)[2].(map[string]any)
	config.ReleaseID, config.Version = forward["releaseId"].(string), forward["version"].(string)
	f.disk.manifest["releaseId"], f.disk.manifest["version"] = config.ReleaseID, config.Version
	componentCatalog := forward["components"].(map[string]any)
	componentCatalog["sha256"], componentCatalog["size"] = r.ComponentsSHA256, r.ComponentsSize
	if update, ok := r.ComponentUpdates["rag"]; ok {
		target := update.Target
		target.Revision = strings.Repeat("f", 64)
		windowsRecoveryCatalog(t, f, forward, target)
	}
	manifest := f.disk.envelope(t, f.policy.private)
	f.disk.writeFile(t, "resources/installed-manifest.json", manifest)
	entry := windowsPolicyManifestArtifact(config.ReleaseID)
	entry.SHA256 = fileManifestEntry("installed-manifest.json", manifest)["sha256"].(string)
	entry.Size = int64(len(manifest))
	forward["installedManifest"] = entry
	if approved {
		original := f.policy.policy["edges"].([]any)[0].(map[string]any)["componentTransitions"].(map[string]any)["rag"].(map[string]any)
		transition := map[string]any{"rag": map[string]any{"sourceClasses": original["sourceClasses"],
			"targetComponentsSHA256": componentCatalog["sha256"]}}
		f.policy.policy["repairAcceptances"] = []any{map[string]any{
			"sourceReleaseId": r.SourceReleaseID, "failedTargetReleaseId": r.TargetReleaseID, "repairReleaseId": config.ReleaseID,
			"sourceDataProfile": r.SourceDataProfile, "targetDataProfile": r.TargetDataProfile,
			"componentTransitions": transition, "targetComponentsSHA256": componentCatalog["sha256"],
		}}
	}
	sequence := r.PolicySequence + 1
	f.policy.policy["policySequence"] = sequence
	decision, err := Check(t.Context(), f.policy.envelope(t), f.policy.config, f.policy.state, f.native.root, false, policyTime)
	if err != nil || decision.PolicySequence != sequence {
		t.Fatalf("accept updated signed policy: %+v, %v", decision, err)
	}
	return config
}

func TestWindowsPolicyRecoveryAuthorizesReinstalledTarget(t *testing.T) {
	for _, version := range []string{"original target", "signed forward target"} {
		t.Run(version, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "repair")
			before := makeWindowsRecoveryRecord(t, f)
			addWindowsRecoverySelection(t, f, before)
			config := f.policy.config
			config.ReleaseID, config.Version = before.TargetReleaseID, before.TargetVersion
			if version == "signed forward target" {
				config = windowsRecoveryForwardBundle(t, f, before, true)
			}
			// A manual installer may be run after the updater's cache is lost.
			for _, name := range []string{before.PackagePath, before.ComponentsPath, before.InstalledManifestPath} {
				if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			oldData := filepath.Join(before.RuntimeRoot, "pre-update-draft")
			const contents = "data saved by the previous release"
			if err := os.WriteFile(oldData, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckStartup(t.Context(), f.native.root, before.Scope); ErrorCode(err) != "update_in_progress" {
				t.Fatalf("repair was open before revalidation: %v", err)
			}
			if err := RecoverWindowsPolicyInstallation(t.Context(), f.native.root, before.InstallationRoot, before.TransactionID, config, strings.Repeat("b", 64), policyTime); err != nil {
				t.Fatalf("verify manually reinstalled signed package: %v", err)
			}
			after, err := ReadNativeRecord(f.native.root)
			if err != nil || after == nil || after.TransactionID != before.TransactionID || !after.WindowsInstallVerified || after.Phase != "installing" {
				t.Fatalf("verified recovery did not establish a startup proof: %+v, %v", after, err)
			}
			id, installedVersion, dataProfile, _ := after.EffectiveTarget()
			if id != config.ReleaseID || installedVersion != config.Version || dataProfile != before.TargetDataProfile ||
				after.RuntimeRoot != before.RuntimeRoot || !reflect.DeepEqual(after.Scope, before.Scope) ||
				!reflect.DeepEqual(after.SourceStorage, before.SourceStorage) || !reflect.DeepEqual(after.WantedComponents, before.WantedComponents) ||
				!reflect.DeepEqual(after.SourceComponents(), before.SourceComponents()) {
				t.Fatalf("repair changed the current target, original storage or selections: %+v", after)
			}
			wantRevision := before.ComponentUpdates["rag"].Target.Revision
			if version == "signed forward target" {
				wantRevision = strings.Repeat("f", 64)
			}
			if after.EffectiveComponents()["rag"].Revision != wantRevision {
				t.Fatalf("recovery did not bind the signed target RAG revision: %+v", after.EffectiveComponents())
			}
			if raw, err := os.ReadFile(oldData); err != nil || string(raw) != contents {
				t.Fatalf("recovery changed original data: %q, %v", raw, err)
			}
			if err := CheckStartup(t.Context(), f.native.root, before.Scope); err != nil {
				t.Fatalf("verified recovery remains blocked at the public launch gate: %v", err)
			}
		})
	}
}

func TestWindowsPolicyRecoveryLeavesTheGateClosedWhenUnsafe(t *testing.T) {
	for _, kind := range []string{"uncertain installer in same boot", "live worker", "wrong scope", "wrong transaction", "unsigned replacement", "unapproved newer release"} {
		t.Run(kind, func(t *testing.T) {
			f := newWindowsPolicyInstallProofFixture(t, "repair")
			r := makeWindowsRecoveryRecord(t, f)
			config := f.policy.config
			config.ReleaseID, config.Version = r.TargetReleaseID, r.TargetVersion
			boot, tx, installation := strings.Repeat("b", 64), r.TransactionID, r.InstallationRoot
			var want string
			switch kind {
			case "uncertain installer in same boot":
				boot, want = r.BootID, "handoff_restart_required"
			case "live worker":
				actor, err := CurrentProcessIdentity()
				if err != nil {
					t.Fatal(err)
				}
				r.HandoffActor = &actor
				if err := writeNativeRecord(f.native.root, r); err != nil {
					t.Fatal(err)
				}
				want = "handoff_state_unknown"
			case "wrong scope":
				installation, want = t.TempDir(), "update_transaction_conflict"
			case "wrong transaction":
				tx, want = strings.Repeat("f", 32), "update_transaction_conflict"
			case "unsigned replacement":
				f.disk.writeFile(t, "resources/app.asar", []byte("changed bytes without changing the signed manifest"))
				want = "installed_manifest_file"
			case "unapproved newer release":
				config = windowsRecoveryForwardBundle(t, f, r, false)
				want = "repair_target_not_accepted"
			}
			before, err := os.ReadFile(f.native.filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := RecoverWindowsPolicyInstallation(t.Context(), f.native.root, installation, tx, config, boot, policyTime); ErrorCode(err) != want {
				t.Errorf("recovery returned %v, want %s", err, want)
			}
			after, err := os.ReadFile(f.native.filename)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected recovery replaced the original transaction: %v", err)
			}
			if err := CheckStartup(t.Context(), f.native.root, r.Scope); ErrorCode(err) != "update_in_progress" {
				t.Fatalf("rejected recovery opened the public launch gate: %v", err)
			}
		})
	}
}
