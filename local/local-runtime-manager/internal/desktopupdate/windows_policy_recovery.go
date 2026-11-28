package desktopupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/releaseversion"
)

// RecoverWindowsPolicyInstallationWithRefresh retries limited policy failures
// using the installed build's policy source. Recovery must still approve the
// same durable transaction against the newly accepted signed envelope.
func RecoverWindowsPolicyInstallationWithRefresh(ctx context.Context, root, installation, tx string, c Config, boot string, client *http.Client, now func() time.Time) error {
	err := RecoverWindowsPolicyInstallation(ctx, root, installation, tx, c, boot, now())
	if err == nil {
		return nil
	}
	switch ErrorCode(err) {
	case "trust_missing", "policy_time", "repair_target_not_accepted", "offer_changed":
		r, readErr := ReadNativeRecord(root)
		if readErr != nil {
			return readErr
		}
		if r == nil || r.TransactionID != tx {
			return failure("update_transaction_conflict", nil)
		}
		if _, refreshErr := Discover(ctx, c, WindowsPolicyRecoverySource(r), root, client, now()); refreshErr != nil {
			return err
		}
		return RecoverWindowsPolicyInstallation(ctx, root, installation, tx, c, boot, now())
	default:
		return err
	}
}

// RecoverWindowsPolicyInstallation proves the manually installed target before
// the launcher starts Electron. The caller supplies the embedded build config
// and an independently observed OS boot session.
func RecoverWindowsPolicyInstallation(ctx context.Context, root, installation, tx string, c Config, boot string, now time.Time) error {
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if c.Mode != "windows-policy" || !filepath.IsAbs(root) || !filepath.IsAbs(installation) {
		return failure("configuration_invalid", nil)
	}
	lock, err := nativeStateLock(ctx, root)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	r, err := ReadNativeRecord(root)
	if err != nil {
		return err
	}
	if err := checkWindowsRecoveryScope(r, installation, tx, c, root, boot); err != nil {
		return err
	}
	envelope, err := ReadAcceptedEnvelope(c, root)
	if err != nil {
		return err
	}
	target, acceptance, err := windowsRecoveredTarget(ctx, root, envelope, c, r, now)
	if err != nil {
		return err
	}
	app := filepath.Dir(filepath.Dir(r.InstallationRoot))
	if r.InstallationRoot != filepath.Join(app, "resources", "runtime") || target.InstalledManifest == nil {
		return failure("path_invalid", nil)
	}
	manifest, err := readInstallProofManifest(ctx, app, filepath.Join("resources", "installed-manifest.json"), target.InstalledManifest.Size)
	if err != nil {
		return err
	}
	if err := VerifyWindowsInstalledManifest(ctx, app, manifest, c, target); err != nil {
		return err
	}
	catalog := filepath.Join(r.InstallationRoot, "config", "python-components.json")
	components, err := readWindowsRecoveryCatalog(ctx, app, target.Components, r)
	if err != nil {
		return err
	}
	if acceptance == "" {
		if !maps.EqualFunc(components, r.targetComponents(), func(a, b ComponentDescriptor) bool { return reflect.DeepEqual(a, b) }) {
			return failure("component_update_incompatible", nil)
		}
		r.ComponentsPath = catalog
	} else {
		repair := &NativeRepairTarget{ReleaseID: target.ID, Version: target.Version, PackageSHA256: target.Package.SHA256,
			ComponentsSHA256: target.Components.SHA256, ComponentsSize: target.Components.Size, ComponentsPath: catalog,
			DataProfile: r.SourceDataProfile, AcceptanceDigest: acceptance,
			InstalledManifestSHA256: target.InstalledManifest.SHA256, InstalledManifestSize: target.InstalledManifest.Size,
			TargetComponents: components}
		if !validNativeRepairComponents(r, repair) {
			return failure("component_update_incompatible", nil)
		}
		r.RepairTarget = repair
	}
	r.Phase, r.WindowsInstallVerified, r.Reason = "installing", true, ""
	r.Owner, r.TargetActor, r.Activation, r.ComponentStage = "", nil, nil, nil
	if err := validateNativeRecord(r); err != nil {
		return err
	}
	ledgerLock, err := lockPolicyLedger(ctx, policyLedgerDirectory(root, c))
	if err != nil {
		return err
	}
	defer releaseLock(ledgerLock)
	current, err := ReadAcceptedEnvelope(c, root)
	if err != nil || !bytes.Equal(current, envelope) {
		return failure("offer_changed", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeNativeRecord(root, r); err != nil {
		return failure("update_record_write", err)
	}
	return nil
}

func checkWindowsRecoveryScope(r *NativeRecord, installation, tx string, c Config, root, boot string) error {
	if r == nil || r.Mode != c.Mode || r.TransactionID != tx || pathKey(r.InstallationRoot) != pathKey(installation) ||
		(r.Phase != "repair" && r.Phase != "installing") || r.WindowsInstallVerified ||
		r.TrustDomain != c.TrustDomain || r.Audience != c.Audience || r.Channel != c.Channel ||
		r.Platform != c.Platform || r.Arch != c.Arch || r.SigningIdentity != c.SigningIdentity {
		return failure("update_transaction_conflict", nil)
	}
	for _, actor := range []*ProcessIdentity{r.SourceActor, r.HandoffActor, r.TargetActor} {
		if actor != nil {
			live, err := SameProcess(*actor)
			if err != nil || live {
				return failure("handoff_state_unknown", err)
			}
		}
	}
	// The supervisor could have died while NSIS was still writing files. A
	// persisted claim alone cannot prove that every installer process ended.
	if r.HandoffActor != nil || r.SourceActor == nil || r.Phase == "installing" {
		return verifyRepairBoot(root, r, boot)
	}
	return nil
}

func windowsRecoveredTarget(ctx context.Context, root string, envelope []byte, c Config, r *NativeRecord, now time.Time) (*Release, string, error) {
	current, version, _, _ := r.EffectiveTarget()
	order, err := releaseversion.Compare(c.Version, version)
	if err != nil || order < 0 || (order == 0 && c.ReleaseID != current) {
		return nil, "", failure("repair_target_not_accepted", err)
	}
	if r.RepairTarget == nil && c.ReleaseID == r.TargetReleaseID {
		if c.Version != r.TargetVersion {
			return nil, "", failure("repair_target_not_accepted", nil)
		}
		target, err := windowsInstallProofTarget(envelope, c, r, now)
		return target, "", err
	}
	local := WindowsPolicyRecoverySource(r)
	_, sourceHadRAG := local.Components["rag"]
	request, err := json.Marshal(map[string]any{"sourceReleaseId": r.SourceReleaseID, "failedTargetReleaseId": r.TargetReleaseID,
		"currentReleaseId": c.ReleaseID, "dataProfile": local.DataProfile, "sourceHadRAG": sourceHadRAG,
		"components": local.Components, "componentMetadata": local.ComponentMetadata})
	if err != nil {
		return nil, "", err
	}
	decision, err := CheckRepairAcceptance(ctx, envelope, c, request, root, now)
	if err != nil {
		return nil, "", err
	}
	if decision.Status != "accepted" || decision.Next == nil || decision.Next.Version != c.Version ||
		decision.PolicySequence < r.PolicySequence || (decision.PolicySequence == r.PolicySequence && decision.PolicyDigest != r.PolicyDigest) {
		return nil, "", failure("repair_target_not_accepted", nil)
	}
	if r.RepairTarget != nil && order == 0 && (decision.Next.Package.SHA256 != r.RepairTarget.PackageSHA256 ||
		decision.Next.Components.SHA256 != r.RepairTarget.ComponentsSHA256 || decision.Next.InstalledManifest == nil ||
		decision.Next.InstalledManifest.SHA256 != r.RepairTarget.InstalledManifestSHA256) {
		return nil, "", failure("offer_changed", nil)
	}
	return decision.Next, decision.PolicyDigest, nil
}

// WindowsPolicyRecoverySource reads selections from the interrupted update,
// so refreshing a policy does not depend on starting the installed runtime.
func WindowsPolicyRecoverySource(r *NativeRecord) LocalState {
	source := LocalState{DataProfile: r.SourceDataProfile, ComponentMetadata: r.SourceComponents(), Components: map[string]string{}}
	for id, component := range source.ComponentMetadata {
		source.Components[id] = component.Revision
	}
	return source
}

func readWindowsRecoveryCatalog(ctx context.Context, app string, artifact Artifact, r *NativeRecord) (map[string]ComponentDescriptor, error) {
	if artifact.Size <= 0 || artifact.Size > MaxEnvelopeBytes {
		return nil, failure("artifact_size", nil)
	}
	raw, err := readInstallProofManifest(ctx, app, filepath.Join("resources", "runtime", "config", "python-components.json"), artifact.Size)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, failure("artifact_hash", nil)
	}
	var catalog struct {
		SchemaVersion  int
		Platform, Arch string
		Components     map[string]ComponentDescriptor
	}
	if json.Unmarshal(raw, &catalog) != nil || catalog.SchemaVersion != 1 || catalog.Platform != r.Platform || catalog.Arch != r.Arch || catalog.Components == nil {
		return nil, failure("component_update_incompatible", nil)
	}
	result := make(map[string]ComponentDescriptor)
	source := r.SourceComponents()
	for id, component := range catalog.Components {
		if component.ID != id || !validUpdateComponent(component) || component.Platform != r.Platform || component.Arch != r.Arch ||
			component.SizeBytes <= 0 || component.UnpackedBytes <= 0 {
			return nil, failure("component_update_incompatible", nil)
		}
		if _, selected := source[id]; selected || slices.Contains(r.WantedComponents, id) {
			result[id] = component
		}
	}
	return result, nil
}
