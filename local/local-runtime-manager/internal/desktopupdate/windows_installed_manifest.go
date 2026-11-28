package desktopupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type installedManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type installedFileIdentity struct {
	Volume uint64
	ID     [16]byte
}

type windowsInstalledManifest struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Kind          string                   `json:"kind"`
	TrustDomain   string                   `json:"trustDomain"`
	Audience      string                   `json:"audience"`
	Channel       string                   `json:"channel"`
	ReleaseID     string                   `json:"releaseId"`
	Version       string                   `json:"version"`
	Platform      string                   `json:"platform"`
	Arch          string                   `json:"arch"`
	Files         []installedManifestEntry `json:"files"`
}

// VerifyWindowsInstalledManifest verifies the installed files against the
// update policy's already validated target, or against the local build identity
// when checking an offline installation without a pending update. The caller
// is responsible for binding root to the selected installation.
func VerifyWindowsInstalledManifest(ctx context.Context, root string, raw []byte, c Config, target *Release) error {
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if c.Mode != "windows-policy" {
		return failure("configuration_invalid", nil)
	}
	payload, err := decodeSignedPayload(raw, c)
	if err != nil {
		return failure("installed_manifest_invalid", err)
	}
	releaseID, version := c.ReleaseID, c.Version
	if target != nil {
		artifact := target.InstalledManifest
		hash := sha256.Sum256(raw)
		if artifact == nil || artifact.Size != int64(len(raw)) || artifact.SHA256 != hex.EncodeToString(hash[:]) ||
			target.Platform != c.Platform || target.Arch != c.Arch || target.SigningIdentity != c.SigningIdentity ||
			target.UpdaterProtocol != c.UpdaterProtocol || !identifier.MatchString(target.ID) {
			return failure("installed_manifest_invalid", nil)
		}
		releaseID, version = target.ID, target.Version
	}
	var manifest windowsInstalledManifest
	if err := decodeStrict(payload, &manifest); err != nil {
		return failure("installed_manifest_invalid", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Kind != "windows-installed-manifest" ||
		manifest.TrustDomain != c.TrustDomain || manifest.Audience != c.Audience || manifest.Channel != c.Channel ||
		manifest.Platform != c.Platform || manifest.Arch != c.Arch || manifest.ReleaseID != releaseID || manifest.Version != version {
		return failure("installed_manifest_invalid", nil)
	}
	if err := verifyWindowsInstalledFiles(ctx, root, manifest.Files); err != nil {
		return failure("installed_manifest_file", err)
	}
	return nil
}

func verifyWindowsInstalledFiles(ctx context.Context, directory string, files []installedManifestEntry) error {
	seen := make(map[string]bool, len(files))
	for _, entry := range files {
		if !fs.ValidPath(entry.Path) || entry.Path == "." || strings.ContainsAny(entry.Path, "\\:<>\"|?*\x00") ||
			entry.Size < 0 || !digestPattern.MatchString(entry.SHA256) {
			return errors.New("invalid installed file entry")
		}
		for _, part := range strings.Split(entry.Path, "/") {
			if strings.TrimRight(part, " .") != part {
				return errors.New("noncanonical installed file entry")
			}
		}
		folded := strings.ToUpper(entry.Path)
		if seen[folded] {
			return errors.New("duplicate installed file entry")
		}
		seen[folded] = true
	}
	for _, required := range []string{
		"LazyMind.exe", "LazyMindApp.exe", "resources/app.asar",
		"resources/runtime/bin/lazymind-update-helper.exe",
		"resources/runtime/bin/local-runtime-manager.exe",
		"resources/runtime/config/python-components.json",
	} {
		if !seen[strings.ToUpper(required)] {
			return errors.New("required installed file entry is missing")
		}
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("invalid installation directory")
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() {
		return errors.New("installation directory is unavailable or aliased")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	identities := make(map[installedFileIdentity]bool, len(files))
	for _, entry := range files {
		if err := verifyWindowsInstalledFile(ctx, root, entry, identities); err != nil {
			return err
		}
	}
	return nil
}

func verifyWindowsInstalledFile(ctx context.Context, root *os.Root, entry installedManifestEntry, identities map[installedFileIdentity]bool) error {
	name, err := filepath.Localize(entry.Path)
	if err != nil {
		return err
	}
	for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
		if info, err := root.Lstat(parent); err != nil || !info.IsDir() {
			return errors.New("installed file parent is unavailable or aliased")
		}
	}
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
		return errors.New("installed file is unavailable or has an unexpected size or type")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != entry.Size {
		return errors.New("installed file changed before verification")
	}
	identity, err := readInstalledFileIdentity(file, opened)
	if err != nil {
		return err
	}
	if identities[identity] {
		return errors.New("installed file aliases another signed entry")
	}
	identities[identity] = true
	hash := sha256.New()
	reader := contextReader{ctx: ctx, reader: file}
	n, err := io.Copy(hash, io.LimitReader(reader, entry.Size))
	if err != nil {
		return err
	}
	var extra [1]byte
	_, end := reader.Read(extra[:])
	if end != nil && !errors.Is(end, io.EOF) {
		return end
	}
	if end == nil || n != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return errors.New("installed file does not match its signed entry")
	}
	return nil
}
