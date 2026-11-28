package desktopupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// VerifyAndRecordWindowsPolicyInstallation is called by the source updater
// after its installer has exited. It does not launch or activate the target.
func VerifyAndRecordWindowsPolicyInstallation(ctx context.Context, root string, c Config, tx string, now time.Time) error {
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if c.Mode != "windows-policy" || !filepath.IsAbs(root) {
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
	if r == nil || r.TransactionID != tx || r.Mode != c.Mode || r.Phase != "installing" || r.RepairTarget != nil ||
		r.SourceReleaseID != c.ReleaseID || r.SourceVersion != c.Version || r.TrustDomain != c.TrustDomain ||
		r.Audience != c.Audience || r.Channel != c.Channel || r.SigningIdentity != c.SigningIdentity ||
		r.Platform != c.Platform || r.Arch != c.Arch {
		return failure("update_transaction_conflict", nil)
	}
	if r.WindowsInstallVerified {
		// A repeated verification must not leave an earlier proof usable if
		// the installation no longer matches.
		r.WindowsInstallVerified = false
		if err := writeNativeRecord(root, r); err != nil {
			return failure("update_record_write", err)
		}
	}
	envelope, err := ReadAcceptedEnvelope(c, root)
	if err != nil {
		return err
	}
	target, err := windowsInstallProofTarget(envelope, c, r, now)
	if err != nil {
		return err
	}
	if err := verifyWindowsInstallProofFiles(ctx, root, c, r, target); err != nil {
		return err
	}
	current, err := ReadAcceptedEnvelope(c, root)
	if err != nil || !bytes.Equal(current, envelope) {
		return failure("offer_changed", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.WindowsInstallVerified = true
	if err := writeNativeRecord(root, r); err != nil {
		return failure("update_record_write", err)
	}
	return nil
}

func windowsInstallProofTarget(envelope []byte, c Config, record *NativeRecord, now time.Time) (*Release, error) {
	policy, payload, err := decodeSigned(envelope, c)
	if err != nil {
		return nil, err
	}
	if err := validatePolicy(&policy, c, now); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	if policy.Sequence < record.PolicySequence || (policy.Sequence == record.PolicySequence && hex.EncodeToString(digest[:]) != record.PolicyDigest) ||
		policy.MinUpdaterProtocol > c.UpdaterProtocol || slices.Contains(policy.Revoked, record.SourceReleaseID) || slices.Contains(policy.Revoked, record.TargetReleaseID) {
		return nil, failure("offer_changed", nil)
	}
	source := LocalState{ComponentMetadata: record.SourceComponents(), Components: map[string]string{}}
	for id, component := range source.ComponentMetadata {
		source.Components[id] = component.Revision
	}
	if !slices.ContainsFunc(policy.Edges, func(edge Edge) bool {
		return edge.SourceID == record.SourceReleaseID && edge.TargetID == record.TargetReleaseID &&
			edge.TargetDataProfile == record.TargetDataProfile && slices.Contains(edge.SourceDataProfiles, record.SourceDataProfile) &&
			matchSourceComponents(edge.ComponentTransitions, source, c) == ""
	}) {
		return nil, failure("offer_changed", nil)
	}
	index := slices.IndexFunc(policy.Releases, func(release Release) bool { return release.ID == record.TargetReleaseID })
	if index < 0 {
		return nil, failure("offer_changed", nil)
	}
	target := &policy.Releases[index]
	if target.Version != record.TargetVersion || target.SigningIdentity != c.SigningIdentity ||
		target.Package.SHA256 != record.PackageSHA256 || target.Package.Size != record.PackageSize || target.Package.UnpackedBytes != record.PackageUnpackedBytes ||
		target.Components.SHA256 != record.ComponentsSHA256 || target.Components.Size != record.ComponentsSize ||
		target.InstalledManifest == nil || target.InstalledManifest.SHA256 != record.InstalledManifestSHA256 ||
		target.InstalledManifest.Size != record.InstalledManifestSize {
		return nil, failure("offer_changed", nil)
	}
	return target, nil
}

func verifyWindowsInstallProofFiles(ctx context.Context, root string, c Config, record *NativeRecord, target *Release) error {
	if err := ValidateWindowsPolicyDownloadCache(root, c); err != nil {
		return err
	}
	cache := DownloadCacheDirectory(root, c)
	filename := target.InstalledManifest.SHA256 + ".json"
	if record.InstalledManifestPath != filepath.Join(cache, filename) {
		return failure("path_invalid", nil)
	}
	cached, err := readInstallProofManifest(ctx, cache, filename, target.InstalledManifest.Size)
	if err != nil {
		return err
	}
	appRoot := filepath.Dir(filepath.Dir(record.InstallationRoot))
	if record.InstallationRoot != filepath.Join(appRoot, "resources", "runtime") {
		return failure("path_invalid", nil)
	}
	installed, err := readInstallProofManifest(ctx, appRoot, filepath.Join("resources", "installed-manifest.json"), target.InstalledManifest.Size)
	if err != nil {
		return err
	}
	if !bytes.Equal(cached, installed) {
		return failure("artifact_hash", nil)
	}
	if err := VerifyWindowsInstalledManifest(ctx, appRoot, installed, c, target); err != nil {
		return err
	}
	return VerifyArtifact(ctx, filepath.Join(record.InstallationRoot, "config", "python-components.json"), target.Components)
}

func readInstallProofManifest(ctx context.Context, directory, name string, size int64) ([]byte, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, failure("artifact_hash", err)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != size {
		return nil, failure("artifact_hash", err)
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: file}, size+1))
	if err != nil || int64(len(raw)) != size {
		return nil, failure("artifact_hash", err)
	}
	return raw, nil
}
