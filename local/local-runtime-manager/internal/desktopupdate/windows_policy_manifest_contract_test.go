//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func windowsPolicyManifestArtifact(id string) Artifact {
	return Artifact{
		URL:    "https://updates.example.invalid/releases/" + id + "/installed-manifest.json",
		SHA256: fixtureArtifact(id + "-installed-manifest")["sha256"].(string),
		Size:   2048,
	}
}

func newWindowsPolicyManifestFixture(t *testing.T) *policyFixture {
	t.Helper()
	f := newV2PolicyFixture(t, false)
	f.config.Mode = "windows-policy"
	f.config.TrustDomain = "fixture-windows-policy"
	f.config.Platform, f.config.Arch = "windows", "amd64"
	f.config.SigningIdentity = "FIXTURE_WINDOWS_RELEASE"
	f.policy["trustDomain"] = f.config.TrustDomain
	f.state.SystemVersion = "10.0.22631"
	for _, entry := range f.policy["releases"].([]any) {
		r := entry.(map[string]any)
		id := r["releaseId"].(string)
		r["platform"], r["arch"] = f.config.Platform, f.config.Arch
		r["minimumSystemVersion"] = "10.0.19045"
		r["signingIdentity"] = f.config.SigningIdentity
		pkg := r["package"].(map[string]any)
		pkg["url"] = "https://updates.example.invalid/releases/" + id + "/package.exe"
		r["installedManifest"] = windowsPolicyManifestArtifact(id)
	}
	return f
}

func TestWindowsPolicyManifestRetainsTargetBinding(t *testing.T) {
	f := newWindowsPolicyManifestFixture(t)
	policy, err := ValidatePublication(f.envelope(t), f.config, policyTime)
	if err != nil {
		t.Fatalf("publication rejected the signed Windows policy: %v (%v)", err, errors.Unwrap(err))
	}
	if len(policy.Releases) != 3 {
		t.Fatalf("publication retained %d releases, want 3", len(policy.Releases))
	}
	for _, release := range policy.Releases {
		assertWindowsPolicyManifestTarget(t, release)
	}
	envelope, err := SignPublication(policy, f.config, "key1", f.private, policyTime)
	if err != nil {
		t.Fatalf("could not publish the validated release set: %v", err)
	}
	decision, err := Check(t.Context(), envelope, f.config, f.state, f.root, false, policyTime)
	if err != nil || decision.Status != "direct" || decision.Next == nil || decision.Next.ID != "r2" {
		t.Fatalf("client did not discover the compatible target: decision=%+v err=%v", decision, err)
	}
	assertWindowsPolicyManifestTarget(t, *decision.Next)
}

func assertWindowsPolicyManifestTarget(t *testing.T, release Release) {
	t.Helper()
	// Inspect the signed JSON contract without requiring a production struct field on the baseline.
	raw, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		InstalledManifest *Artifact `json:"installedManifest"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	if want := windowsPolicyManifestArtifact(release.ID); exported.InstalledManifest == nil || *exported.InstalledManifest != want {
		t.Errorf("release %s installedManifest = %+v, want %+v", release.ID, exported.InstalledManifest, want)
	}
	want := Artifact{
		URL:    "https://updates.example.invalid/releases/" + release.ID + "/package.exe",
		SHA256: fixtureArtifact(release.ID)["sha256"].(string),
		Size:   1024,
	}
	if release.Package != want {
		t.Errorf("release %s package = %+v, want %+v", release.ID, release.Package, want)
	}
}

func TestWindowsPolicyManifestRejectsInvalidReleaseDescriptors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]any)
	}{
		{"all manifests omitted", func(releases []any) {
			for _, entry := range releases {
				delete(entry.(map[string]any), "installedManifest")
			}
		}},
		{"unrouted release missing manifest", func(releases []any) {
			delete(releases[2].(map[string]any), "installedManifest")
		}},
		{"target manifest null", func(releases []any) { releases[1].(map[string]any)["installedManifest"] = nil }},
		{"target manifest bad SHA-256", func(releases []any) {
			descriptor := windowsPolicyManifestArtifact("r2")
			descriptor.SHA256 = strings.Repeat("g", 64)
			releases[1].(map[string]any)["installedManifest"] = descriptor
		}},
		{"target manifest short SHA-256", func(releases []any) {
			descriptor := windowsPolicyManifestArtifact("r2")
			descriptor.SHA256 = descriptor.SHA256[:63]
			releases[1].(map[string]any)["installedManifest"] = descriptor
		}},
		{"target manifest zero size", func(releases []any) {
			descriptor := windowsPolicyManifestArtifact("r2")
			descriptor.Size = 0
			releases[1].(map[string]any)["installedManifest"] = descriptor
		}},
		{"target manifest outside pinned origin", func(releases []any) {
			descriptor := windowsPolicyManifestArtifact("r2")
			descriptor.URL = "https://other.example.invalid/releases/r2/installed-manifest.json"
			releases[1].(map[string]any)["installedManifest"] = descriptor
		}},
		{"unrouted macOS release", func(releases []any) {
			releases[2].(map[string]any)["platform"] = "darwin"
		}},
		{"unrouted Windows arm64 release", func(releases []any) {
			releases[2].(map[string]any)["arch"] = "arm64"
		}},
		{"unrouted prerelease", func(releases []any) {
			releases[2].(map[string]any)["version"] = "1.2.0-rc.1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsPolicyManifestFixture(t)
			tc.change(f.policy["releases"].([]any))
			assertWindowsPolicyManifestRejected(t, f)
		})
	}
}

func TestWindowsPolicyManifestIsRejectedByOtherProfiles(t *testing.T) {
	for _, profile := range []struct {
		name, platform, channel string
		protocol                int
	}{
		{"official Authenticode Windows V2", "windows", "stable", 2},
		{"QA Authenticode Windows V2", "windows", "canary", 2},
		{"official macOS V2", "darwin", "stable", 2},
		{"official macOS V1", "darwin", "stable", 1},
	} {
		t.Run(profile.name, func(t *testing.T) {
			var f *policyFixture
			switch {
			case profile.platform == "windows":
				f = newWindowsPolicyManifestFixture(t)
				f.config.Mode = "official"
			case profile.protocol == 1:
				f = newPolicyFixture(t)
			default:
				f = newV2PolicyFixture(t, false)
			}
			f.config.Channel, f.policy["channel"] = profile.channel, profile.channel
			releases := f.policy["releases"].([]any)
			for _, entry := range releases {
				delete(entry.(map[string]any), "installedManifest")
			}
			envelope := f.envelope(t)
			if _, err := ValidatePublication(envelope, f.config, policyTime); err != nil {
				t.Fatalf("publication rejected the existing profile without the field: %v", err)
			}
			decision, err := Check(t.Context(), envelope, f.config, f.state, f.root, false, policyTime)
			if err != nil || decision.Next == nil || decision.Next.ID != "r2" {
				t.Fatalf("client rejected the existing profile without the field: decision=%+v err=%v", decision, err)
			}
			r := releases[1].(map[string]any)
			t.Run("object", func(t *testing.T) {
				r["installedManifest"] = windowsPolicyManifestArtifact("r2")
				assertWindowsPolicyManifestRejected(t, f)
			})
			t.Run("explicit null", func(t *testing.T) {
				r["installedManifest"] = nil
				assertWindowsPolicyManifestRejected(t, f)
			})
		})
	}
}

func assertWindowsPolicyManifestRejected(t *testing.T, f *policyFixture) {
	t.Helper()
	envelope := f.envelope(t)
	if _, err := ValidatePublication(envelope, f.config, policyTime); err == nil {
		t.Error("publication accepted an invalid signed installed-manifest contract")
	}
	root := t.TempDir()
	if decision, err := Check(t.Context(), envelope, f.config, f.state, root, false, policyTime); err == nil {
		t.Errorf("client accepted an invalid signed installed-manifest contract: %+v", decision)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Errorf("invalid policy changed trust state: entries=%v err=%v", entries, err)
	}
}
