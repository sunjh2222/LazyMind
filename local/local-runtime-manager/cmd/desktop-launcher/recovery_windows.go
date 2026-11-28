package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
	"golang.org/x/sys/windows"
)

// The installed launcher needs its own trusted configuration because an
// interrupted update cannot execute the new installation's identity helper.
var embeddedConfig string

var readLauncherWindowsBootID = func(ctx context.Context) (string, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	command := exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-Command", "(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToFileTimeUtc()")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	raw, err := command.Output()
	if err != nil || len(raw) > 4096 {
		return "", &desktopupdate.Failure{Code: "handoff_state_unknown", Cause: err}
	}
	raw = bytes.TrimSpace(raw)
	stamp, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil || stamp == 0 || strconv.FormatUint(stamp, 10) != string(raw) {
		return "", &desktopupdate.Failure{Code: "handoff_state_unknown", Cause: err}
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func checkWindowsDesktopStartup(launcher string) error {
	err := checkPendingDesktopUpdate(launcher)
	if err == nil || desktopupdate.ErrorCode(err) != "update_in_progress" || embeddedConfig == "" || installerWarmup() {
		return err
	}
	raw, decodeErr := base64.StdEncoding.DecodeString(embeddedConfig)
	var config desktopupdate.Config
	if decodeErr != nil || json.Unmarshal(raw, &config) != nil || config.Mode != "windows-policy" || desktopupdate.ValidateConfig(config) != nil {
		return err
	}
	root, rootErr := resolveLauncherUpdaterRoot()
	if rootErr != nil {
		return rootErr
	}
	r, recordErr := desktopupdate.ReadNativeRecord(root)
	if recordErr != nil {
		return recordErr
	}
	if r == nil || r.Mode != config.Mode || r.WindowsInstallVerified {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	boot, bootErr := readLauncherWindowsBootID(ctx)
	if bootErr != nil {
		return bootErr
	}
	installation := filepath.Join(filepath.Dir(launcher), "resources", "runtime")
	if repairErr := desktopupdate.RecoverWindowsPolicyInstallationWithRefresh(ctx, root, installation, r.TransactionID, config, boot, nil, time.Now); repairErr != nil {
		return repairErr
	}
	return checkPendingDesktopUpdate(launcher)
}

func installerWarmup() bool {
	return slices.Contains(os.Args[1:], "--installer-warmup")
}
