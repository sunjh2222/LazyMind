//go:build !lazymind_unsigned_test && (darwin || windows)

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
)

type windowsManifestBootstrapFixture struct {
	state                                  *updateStateFixture
	root, packagePath, componentPath, path string
	manifest                               []byte
}

func newWindowsManifestBootstrapFixture(t *testing.T, mode string) windowsManifestBootstrapFixture {
	t.Helper()
	f := actualUpdateFormatFixture(t, 2)
	replacementFile(t, filepath.Join(f.paths.ResourcesRoot, "config", "update-format-v2.json"), updateFormatRecipe)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previousRoot := resolveRuntimeRecoveryRoot
	resolveRuntimeRecoveryRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { resolveRuntimeRecoveryRoot = previousRoot })

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	configuration := desktopupdate.Config{
		Mode: mode, TrustDomain: "fixture-windows-policy", Audience: "official", Channel: "stable",
		ReleaseID: "source", Version: "1.0.0", Platform: "windows", Arch: "amd64", SigningIdentity: "fixture", UpdaterProtocol: 2,
		PolicyURL: "https://updates.example.invalid/policy.json", AllowedOrigins: []string{"https://updates.example.invalid"},
		PublicKeys: map[string]string{"fixture": base64.StdEncoding.EncodeToString(publicKey)},
	}
	// The helper replaces only the host identity provider. The signed policy,
	// CLI, database preflight and atomic transaction writer are real.
	configJSON := replacementJSON(t, map[string]any{"status": "enabled", "config": configuration})
	helper := filepath.Join(f.paths.ResourcesRoot, "bin", executableName("lazymind-update-helper"))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "identity.go")
	replacementFile(t, source, []byte(`package main
import "os"
func main() {
	if len(os.Args) != 2 || os.Args[1] != "verify-identity" { os.Exit(2) }
	os.Stdout.Write([]byte(`+strconv.Quote(string(configJSON))+`))
}`))
	if output, err := exec.CommandContext(t.Context(), executablePath(filepath.Join(runtime.GOROOT(), "bin"), "go"), "build", "-o", helper, source).CombinedOutput(); err != nil {
		t.Fatalf("build synthetic identity provider: %v %s", err, output)
	}

	// Match the actual host catalog reader. Program and manifest bytes are
	// synthetic data, bound by the real release signature and never executed.
	catalog := replacementJSON(t, map[string]any{
		"schemaVersion": 1, "platform": runtime.GOOS, "arch": runtime.GOARCH, "components": map[string]any{},
	})
	pkg := []byte("synthetic Windows installer bytes for target")
	manifest := []byte("synthetic installed manifest bytes for target")
	partition := hexDigest([]byte(configuration.TrustDomain + "\x00" + configuration.Audience + "\x00" + configuration.Channel))
	cache := filepath.Join(root, "downloads", partition)
	fixture := windowsManifestBootstrapFixture{
		state: f, root: root, manifest: manifest,
		packagePath: filepath.Join(cache, hexDigest(pkg)+".exe"), componentPath: filepath.Join(cache, hexDigest(catalog)+".json"),
		path: filepath.Join(cache, hexDigest(manifest)+".json"),
	}
	replacementFile(t, fixture.packagePath, pkg)
	replacementFile(t, fixture.componentPath, catalog)
	local := bootstrapPolicyLocalState(f.measure(t))
	now := time.Now().UTC()
	policy := bootstrapPolicyPayload(configuration, map[string]any{
		"sourceDataProfile": local.DataProfile, "targetDataProfile": local.DataProfile,
		"packageSHA256": hexDigest(pkg), "packageSize": len(pkg), "componentsSHA256": hexDigest(catalog), "componentsSize": len(catalog),
	}, 42, now)
	for _, item := range policy["releases"].([]any) {
		release := item.(map[string]any)
		id := release["releaseId"].(string)
		pkgArtifact := release["package"].(map[string]any)
		pkgArtifact["unpackedBytes"] = len(pkg) + 128
		if mode == "windows-policy" {
			body := manifest
			if id == "source" {
				body = []byte("synthetic installed manifest bytes for source")
			}
			release["installedManifest"] = desktopupdate.Artifact{
				URL:    "https://updates.example.invalid/releases/" + id + "/installed-manifest.json",
				SHA256: hexDigest(body), Size: int64(len(body)),
			}
		}
	}
	offer, err := desktopupdate.Check(t.Context(), signBootstrapPolicy(t, privateKey, policy), configuration, local, root, false, now)
	if err != nil || offer.Status != "direct" || offer.Next == nil || offer.Next.ID != "target" {
		t.Fatalf("fixture could not accept the signed target: %+v err=%v", offer, err)
	}
	return fixture
}

func (f windowsManifestBootstrapFixture) command(t *testing.T, action string, flags ...string) map[string]any {
	t.Helper()
	var out, diagnostic bytes.Buffer
	cli := NewCLI(&out, &diagnostic, &ExecRunner{}, "fixture-manager")
	args := []string{"update-bootstrap", "--action", action, "--profile", "desktop",
		"--repo-root", f.state.cfg.RepoRoot, "--resources-root", f.state.paths.ResourcesRoot,
		"--runtime-root", f.state.paths.RuntimeRoot, "--owner-token", f.state.cfg.OwnerToken}
	if err := cli.Run(t.Context(), append(args, flags...)); err != nil {
		t.Fatalf("%s CLI failed: %v (%s)", action, err, diagnostic.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode %s reply: %v (%s)", action, err, out.String())
	}
	return result
}

func (f windowsManifestBootstrapFixture) prepare(t *testing.T, flags ...string) map[string]any {
	t.Helper()
	return f.command(t, "prepare-components", append([]string{"--release-id", "target", "--package", f.packagePath, "--components", f.componentPath}, flags...)...)
}

func TestWindowsPolicyManifestBootstrapCLI(t *testing.T) {
	t.Run("legacy fixture prepares without a manifest", func(t *testing.T) {
		f := newWindowsManifestBootstrapFixture(t, "official")
		if result := f.prepare(t); result["status"] != "prepared" {
			t.Fatalf("legacy fixture preparation failed: %v", result)
		}
	})
	t.Run("binds and rechecks exact manifest bytes", func(t *testing.T) {
		f := newWindowsManifestBootstrapFixture(t, "windows-policy")
		replacementFile(t, f.path, f.manifest)
		if result := f.prepare(t, "--installed-manifest", f.path); result["status"] != "prepared" {
			t.Fatalf("manifest preparation failed: %v", result)
		}
		record, err := desktopupdate.ReadNativeRecord(f.root)
		if err != nil || record == nil || record.Phase != "pending" || record.TargetReleaseID != "target" {
			t.Fatalf("read pending target: %+v err=%v", record, err)
		}
		var binding struct {
			Mode string `json:"mode"`
			Path string `json:"installedManifestPath"`
			SHA  string `json:"installedManifestSHA256"`
			Size int64  `json:"installedManifestSize"`
		}
		if err := json.Unmarshal(replacementJSON(t, record), &binding); err != nil || binding.Mode != "windows-policy" ||
			binding.Path != f.path || binding.SHA != hexDigest(f.manifest) || binding.Size != int64(len(f.manifest)) {
			t.Fatalf("durable installed manifest binding changed: %+v err=%v", binding, err)
		}
		verify := func() map[string]any {
			return f.command(t, "verify-components", "--transaction-id", record.TransactionID)
		}
		if result := verify(); result["status"] != "compatible" {
			t.Fatalf("exact cached manifest was rejected before handoff: %v", result)
		}
		changed := bytes.Clone(f.manifest)
		changed[0] ^= 1
		replacementFile(t, f.path, changed)
		if result := verify(); result["status"] != "blocked" || result["reason"] != "artifact_hash" {
			t.Fatalf("same-length corrupt manifest passed pre-handoff verification: %v", result)
		}
		after, err := desktopupdate.ReadNativeRecord(f.root)
		if err != nil || after == nil || !bytes.Equal(replacementJSON(t, after), replacementJSON(t, record)) {
			t.Fatalf("failed verification changed the pending transaction: %+v err=%v", after, err)
		}
	})
	t.Run("corrupt manifest cannot create a transaction", func(t *testing.T) {
		f := newWindowsManifestBootstrapFixture(t, "windows-policy")
		changed := bytes.Clone(f.manifest)
		changed[0] ^= 1
		replacementFile(t, f.path, changed)
		if result := f.prepare(t, "--installed-manifest", f.path); result["status"] != "blocked" || result["reason"] != "artifact_hash" {
			t.Fatalf("corrupt manifest passed preparation: %v", result)
		}
		if record, err := desktopupdate.ReadNativeRecord(f.root); err != nil || record != nil {
			t.Fatalf("failed preparation left a native transaction: %+v err=%v", record, err)
		}
	})
}
