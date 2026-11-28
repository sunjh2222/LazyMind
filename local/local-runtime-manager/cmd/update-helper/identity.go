package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	update "github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
)

// Build tooling may inspect config before packaging. Runtime authorization uses
// this separate command and checks the installed app's actual signing identity.
func verifyInstalledIdentity(ctx context.Context, encoded string) error {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	var c update.Config
	if err != nil || json.Unmarshal(raw, &c) != nil || update.ValidateConfig(c) != nil {
		return errors.New("build_identity_mismatch")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	resources := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	// Deep verification competes with the other packaged service startups.
	bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		app := filepath.Clean(filepath.Join(resources, "..", ".."))
		if update.UnsignedTestBuild {
			paths, systemApps, err := update.UnsignedApplicationLocations()
			if err != nil || !update.IsUnsignedApplication(app, paths, systemApps) || update.VerifyUnsignedMacBundle(bounded, app, c) != nil {
				return errors.New("build_identity_mismatch")
			}
			return nil
		}
		if filepath.Ext(app) != ".app" {
			return errors.New("build_identity_mismatch")
		}
		if exec.CommandContext(bounded, "/usr/bin/codesign", "--verify", "--deep", "--strict", app).Run() != nil {
			return errors.New("build_identity_mismatch")
		}
		command := exec.CommandContext(bounded, "/usr/bin/codesign", "-dv", "--verbose=4", app)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "Authority=Developer ID Application:") || !strings.Contains(string(output), "TeamIdentifier="+c.SigningIdentity+"\n") {
			return errors.New("build_identity_mismatch")
		}
		return nil
	}
	if runtime.GOOS == "windows" {
		if c.Mode == "windows-policy" {
			return verifyWindowsPolicyInstalledIdentity(bounded, executable, c)
		}
		app := filepath.Join(filepath.Dir(resources), "LazyMind.exe")
		script := "$ErrorActionPreference='Stop';foreach($p in @($env:LM_VERIFY_APP,$env:LM_VERIFY_HELPER)){$s=Get-AuthenticodeSignature -LiteralPath $p;if($s.Status -ne 'Valid'){exit 1};if($s.SignerCertificate.GetNameInfo([System.Security.Cryptography.X509Certificates.X509NameType]::SimpleName,$false) -ne $env:LM_VERIFY_SIGNER){exit 1}}"
		command := exec.CommandContext(bounded, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		command.Env = append(os.Environ(), "LM_VERIFY_APP="+app, "LM_VERIFY_HELPER="+executable, "LM_VERIFY_SIGNER="+c.SigningIdentity)
		if command.Run() != nil {
			return errors.New("build_identity_mismatch")
		}
		return nil
	}
	return errors.New("build_identity_mismatch")
}

func verifyWindowsPolicyInstalledIdentity(ctx context.Context, executable string, c update.Config) error {
	bin := filepath.Dir(executable)
	resources := filepath.Dir(filepath.Dir(bin))
	app := filepath.Dir(resources)
	expected := filepath.Join(app, "resources", "runtime", "bin", "lazymind-update-helper.exe")
	if !filepath.IsAbs(executable) || !strings.EqualFold(executable, expected) {
		return errors.New("build_identity_mismatch")
	}
	root, err := os.OpenRoot(app)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Join("resources", "installed-manifest.json")
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > update.MaxEnvelopeBytes {
		return errors.New("build_identity_mismatch")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("build_identity_mismatch")
	}
	manifest, err := io.ReadAll(io.LimitReader(file, update.MaxEnvelopeBytes+1))
	if err != nil || int64(len(manifest)) != info.Size() {
		return errors.New("build_identity_mismatch")
	}
	return update.VerifyWindowsInstalledManifest(ctx, app, manifest, c, nil)
}
