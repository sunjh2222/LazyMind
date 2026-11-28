//go:build lazymind_unsigned_test

package desktopupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"testing"
)

func TestWindowsPolicyManifestIsRejectedByIsolatedV2(t *testing.T) {
	f := newV2PolicyFixture(t, false)
	f.config = unsignedConfig(t)
	f.config.PublicKeys = map[string]string{
		"key1": base64.StdEncoding.EncodeToString(f.private.Public().(ed25519.PublicKey)),
	}
	f.policy["trustDomain"], f.policy["audience"], f.policy["channel"] = f.config.TrustDomain, f.config.Audience, f.config.Channel
	releases := f.policy["releases"].([]any)
	source := releases[0].(map[string]any)
	source["releaseId"], source["version"] = f.config.ReleaseID, f.config.Version
	f.policy["edges"].([]any)[0].(map[string]any)["sourceReleaseId"] = f.config.ReleaseID
	for _, entry := range releases {
		r := entry.(map[string]any)
		id := r["releaseId"].(string)
		r["signingIdentity"] = f.config.SigningIdentity
		for key, name := range map[string]string{"package": "package.zip", "feed": "feed.json", "components": "components.json"} {
			r[key].(map[string]any)["url"] = "releases/" + id + "/" + name
		}
	}

	envelope := f.envelope(t)
	if _, err := ValidatePublication(envelope, f.config, policyTime); err != nil {
		t.Fatalf("publication rejected an existing isolated policy without the field: %v", err)
	}
	decision, err := Check(t.Context(), envelope, f.config, f.state, f.root, false, policyTime)
	if err != nil || decision.Status != "direct" || decision.Next == nil || decision.Next.ID != "r2" {
		t.Fatalf("client rejected an existing isolated policy without the field: decision=%+v err=%v", decision, err)
	}

	for _, tc := range []struct {
		name       string
		descriptor any
	}{
		{"object", Artifact{
			URL:    "releases/r2/installed-manifest.json",
			SHA256: fixtureArtifact("r2-installed-manifest")["sha256"].(string),
			Size:   2048,
		}},
		{"explicit null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases[1].(map[string]any)["installedManifest"] = tc.descriptor
			envelope := f.envelope(t)
			if _, err := ValidatePublication(envelope, f.config, policyTime); err == nil {
				t.Error("publication accepted installedManifest in an isolated policy")
			}
			root := t.TempDir()
			if _, err := Check(t.Context(), envelope, f.config, f.state, root, false, policyTime); err == nil {
				t.Error("client accepted installedManifest in an isolated policy")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Errorf("invalid policy changed isolated trust state: entries=%v err=%v", entries, err)
			}
		})
	}
}
