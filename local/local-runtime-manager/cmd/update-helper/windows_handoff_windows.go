package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// electron-builder 24.13.3 derives this key from the desktop app ID.
const windowsInstallRegistryKey = `Software\412e5ec7-d7ee-55e2-b3e8-1ea8840a484e`

var readWindowsPolicyInstallLocation = func() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsInstallRegistryKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	location, kind, err := key.GetStringValue("InstallLocation")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err == nil && kind != registry.SZ {
		return "", errors.New("unexpected InstallLocation type")
	}
	return location, err
}

var readWindowsPolicyUserProgramFiles = func() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_UserProgramFiles, windows.KF_FLAG_CREATE)
}

type windowsHandoffReply struct {
	Status        string `json:"status"`
	TransactionID string `json:"transactionId"`
	Reason        string `json:"reason,omitempty"`
}

func windowsHandoffError(code string, err error) error {
	return &desktopupdate.Failure{Code: code, Cause: err}
}

func checkWindowsInstallDestination(root, installation string) error {
	location, err := readWindowsPolicyInstallLocation()
	if err != nil {
		return windowsHandoffError("native_install_location_mismatch", err)
	}
	defaultPrograms := filepath.Join(filepath.Dir(root), "Programs")
	if location == "" {
		programs, err := readWindowsPolicyUserProgramFiles()
		if err != nil {
			programs = defaultPrograms
		}
		location = filepath.Join(programs, "LazyMind")
	}
	// The existing NSIS maintenance helper only permits the per-user LazyMind
	// program directory. Check it as well as NSIS's actual selected directory.
	app := filepath.Join(defaultPrograms, "LazyMind")
	if !filepath.IsAbs(location) || !strings.EqualFold(filepath.Clean(location), app) ||
		!strings.EqualFold(installation, filepath.Join(app, "resources", "runtime")) {
		return windowsHandoffError("native_install_location_mismatch", nil)
	}
	return nil
}

func runWindowsHandoff(ctx context.Context, args []string, output io.Writer, c desktopupdate.Config, root string) (result error) {
	if c.Mode != "windows-policy" || !filepath.IsAbs(root) {
		return windowsHandoffError("native_install_unavailable", nil)
	}
	record, err := desktopupdate.ReadNativeRecord(root)
	if err != nil {
		return err
	}
	if record == nil || record.TransactionID != args[1] || record.Mode != c.Mode || record.Phase != "installing" || record.SourceActor == nil ||
		record.SourceReleaseID != c.ReleaseID || record.SourceVersion != c.Version || record.HandoffActor != nil || record.WindowsInstallVerified {
		return windowsHandoffError("update_transaction_conflict", nil)
	}
	helper := filepath.Join(record.InstallationRoot, "bin", "lazymind-update-helper.exe")
	worker := filepath.Join(root, "windows-update-worker.exe")
	self, err := os.Executable()
	if err != nil {
		return windowsHandoffError("native_install_unavailable", err)
	}
	parent, err := desktopupdate.IdentifyProcess(os.Getppid())
	if err != nil {
		return err
	}
	app := filepath.Dir(filepath.Dir(record.InstallationRoot))
	sourceHandle, err := openWindowsHandoffProcess(*record.SourceActor, filepath.Join(app, "LazyMindApp.exe"))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sourceHandle)
	if args[0] == "windows-handoff" {
		if parent != *record.SourceActor || !strings.EqualFold(self, helper) {
			return windowsHandoffError("update_transaction_conflict", nil)
		}
		defer func() {
			if result != nil {
				err := desktopupdate.MarkWindowsPolicyHandoffSourceRejected(context.WithoutCancel(ctx), root,
					record.TransactionID, parent, desktopupdate.ErrorCode(result))
				result = errors.Join(result, err)
			}
		}()
		if err := checkWindowsInstallDestination(root, record.InstallationRoot); err != nil {
			return err
		}
		return startWindowsHandoffWorker(ctx, output, c, root, helper, worker, record.TransactionID)
	}
	if !strings.EqualFold(self, worker) || parent == *record.SourceActor {
		return windowsHandoffError("update_transaction_conflict", nil)
	}
	parentHandle, err := openWindowsHandoffProcess(parent, helper)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parentHandle)
	if err := checkWindowsInstallDestination(root, record.InstallationRoot); err != nil {
		return err
	}
	actor, err := desktopupdate.CurrentProcessIdentity()
	if err != nil {
		return err
	}
	var releaseTarget func()
	defer func() {
		if releaseTarget != nil {
			releaseTarget()
		}
	}()
	return desktopupdate.RunWindowsPolicyHandoff(ctx, root, c, record.TransactionID, *record.SourceActor, actor,
		func() error {
			return json.NewEncoder(output).Encode(windowsHandoffReply{Status: "handoff_accepted", TransactionID: record.TransactionID})
		},
		func(ctx context.Context, _ desktopupdate.ProcessIdentity) error {
			// Allow the dispatcher its entire reply budget before timing out the
			// source. The installer still requires both handles to signal exit.
			wait, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			if err := waitWindowsHandoffProcess(wait, parentHandle); err != nil {
				return err
			}
			return waitWindowsHandoffProcess(wait, sourceHandle)
		},
		func(ctx context.Context, filename string, flags []string) error {
			if err := runWindowsHandoffInstaller(ctx, root, c, filename, flags, record, actor); err != nil {
				return err
			}
			// Keep the public launcher stable from the installed-file proof
			// until Windows has created the target process.
			var err error
			releaseTarget, err = pinWindowsHandoffPath(filepath.Join(app, "LazyMind.exe"), false)
			return err
		},
		launchWindowsHandoffTarget,
	)
}

func openWindowsHandoffProcess(actor desktopupdate.ProcessIdentity, filename string) (windows.Handle, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(actor.PID))
	if err != nil {
		return 0, windowsHandoffError("process_identity", err)
	}
	var creation, exit, kernel, user windows.Filetime
	err = windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user)
	if err == nil && uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime) == actor.StartID {
		buffer := make([]uint16, 32768)
		length := uint32(len(buffer))
		err = windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil && strings.EqualFold(windows.UTF16ToString(buffer[:length]), filename) {
			return handle, nil
		}
	}
	_ = windows.CloseHandle(handle)
	return 0, windowsHandoffError("process_identity", err)
}

func waitWindowsHandoffProcess(ctx context.Context, handle windows.Handle) error {
	for {
		if err := ctx.Err(); err != nil {
			return windowsHandoffError("native_handoff_unknown", err)
		}
		state, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			return windowsHandoffError("native_handoff_unknown", err)
		}
		switch state {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
		default:
			return windowsHandoffError("native_handoff_unknown", nil)
		}
	}
}

func startWindowsHandoffWorker(ctx context.Context, output io.Writer, c desktopupdate.Config, root, helper, worker, tx string) (result error) {
	stageRoot, err := pinWindowsHandoffPath(root, true)
	if err != nil {
		return err
	}
	defer stageRoot()
	artifact, err := stageWindowsHandoffWorker(ctx, c, helper, worker)
	if err != nil {
		return err
	}
	staged, err := pinWindowsHandoffPath(worker, false)
	if err != nil {
		return err
	}
	defer staged()
	if err := desktopupdate.VerifyArtifact(ctx, worker, artifact); err != nil {
		return err
	}
	command := exec.Command(worker, "windows-handoff-worker", tx)
	command.Dir = root
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	pipe, err := command.StdoutPipe()
	if err != nil {
		return windowsHandoffError("native_install_failed", err)
	}
	defer pipe.Close()
	if err := command.Start(); err != nil {
		return windowsHandoffError("native_install_failed", err)
	}
	actor, actorErr := desktopupdate.IdentifyProcess(command.Process.Pid)
	accepted := false
	defer func() {
		if accepted {
			_ = command.Process.Release()
		} else {
			_ = command.Process.Kill()
			_ = command.Wait()
			if actorErr == nil {
				result = errors.Join(result, desktopupdate.MarkWindowsPolicyHandoffInterrupted(context.WithoutCancel(ctx), root, tx, actor, "native_handoff_unknown"))
			}
		}
	}()
	replies := make(chan windowsHandoffReply, 1)
	go func() {
		var reply windowsHandoffReply
		frame, err := bufio.NewReaderSize(pipe, 4096).ReadSlice('\n')
		if err != nil || json.Unmarshal(frame, &reply) != nil {
			reply = windowsHandoffReply{}
		}
		replies <- reply
	}()
	select {
	case <-ctx.Done():
		return windowsHandoffError("native_handoff_unknown", ctx.Err())
	case reply := <-replies:
		if reply.Status == "check_failed" && reply.Reason != "" {
			return windowsHandoffError(reply.Reason, nil)
		}
		if reply.Status != "handoff_accepted" || reply.TransactionID != tx {
			return windowsHandoffError("native_handoff_unknown", nil)
		}
		liveActor, liveErr := desktopupdate.IdentifyProcess(command.Process.Pid)
		record, recordErr := desktopupdate.ReadNativeRecord(root)
		if actorErr != nil || liveErr != nil || liveActor != actor || recordErr != nil || record == nil || record.TransactionID != tx || record.Phase != "installing" ||
			record.HandoffActor == nil || *record.HandoffActor != actor || ctx.Err() != nil {
			return windowsHandoffError("native_handoff_unknown", errors.Join(actorErr, liveErr, recordErr, ctx.Err()))
		}
		if err := json.NewEncoder(output).Encode(reply); err != nil {
			return windowsHandoffError("native_handoff_unknown", err)
		}
		accepted = true
		return nil
	}
}

func stageWindowsHandoffWorker(ctx context.Context, c desktopupdate.Config, helper, worker string) (desktopupdate.Artifact, error) {
	var artifact desktopupdate.Artifact
	keepSource, err := pinWindowsHandoffPath(helper, false)
	if err != nil {
		return artifact, err
	}
	defer keepSource()
	if err := verifyWindowsPolicyInstalledIdentity(ctx, helper, c); err != nil {
		return artifact, windowsHandoffError("build_identity_mismatch", err)
	}
	source, err := os.Open(helper)
	if err != nil {
		return artifact, err
	}
	defer source.Close()
	file, err := os.CreateTemp(filepath.Dir(worker), "windows-update-worker-*.exe")
	if err != nil {
		return artifact, err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), source)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return artifact, errors.Join(err, closeErr)
	}
	if err := os.Rename(file.Name(), worker); err != nil {
		return artifact, err
	}
	return desktopupdate.Artifact{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}, nil
}

func runWindowsHandoffInstaller(ctx context.Context, root string, c desktopupdate.Config, filename string, flags []string, record *desktopupdate.NativeRecord, worker desktopupdate.ProcessIdentity) error {
	if filename != record.PackagePath {
		return windowsHandoffError("update_transaction_conflict", nil)
	}
	keepPackage, err := pinWindowsHandoffPath(filename, false)
	if err != nil {
		return err
	}
	defer keepPackage()
	artifact := desktopupdate.Artifact{SHA256: record.PackageSHA256, Size: record.PackageSize}
	if err := desktopupdate.VerifyArtifact(ctx, filename, artifact); err != nil {
		return err
	}
	// Observe the actual result. Killing NSIS on a deadline can interrupt an
	// overwrite, so let it exit and check the context before granting proof.
	command := exec.Command(filename, flags...)
	command.Dir = filepath.Dir(filename)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := desktopupdate.StartWindowsPolicyInstaller(ctx, root, c, *record, worker, func() error {
		if err := checkWindowsInstallDestination(root, record.InstallationRoot); err != nil {
			return err
		}
		return command.Start()
	}); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return waitForWindowsPolicyInstaller(wait, command.Wait, func(persist context.Context) error {
		return desktopupdate.MarkWindowsPolicyHandoffInterrupted(persist, root, record.TransactionID, worker, "native_install_timeout")
	})
}

func launchWindowsHandoffTarget(executable string) error {
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := command.Start(); err != nil {
		return windowsHandoffError("native_launch_failed", err)
	}
	_ = command.Process.Release()
	return nil
}

// Pin every directory and the leaf through the checked process creation. The
// share mode permits reads but prevents leaf updates and path redirection.
func pinWindowsHandoffPath(filename string, directory bool) (release func(), err error) {
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename {
		return nil, windowsHandoffError("path_invalid", nil)
	}
	var handles []windows.Handle
	cleanup := func() {
		for _, handle := range handles {
			_ = windows.CloseHandle(handle)
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	paths := []string{filename}
	for parent := filepath.Dir(filename); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		paths = append(paths, parent)
	}
	for i := len(paths) - 1; i >= 0; i-- {
		name, err := windows.UTF16PtrFromString(paths[i])
		if err != nil {
			return nil, windowsHandoffError("path_invalid", err)
		}
		isDir := i > 0 || directory
		access, share := uint32(windows.GENERIC_READ), uint32(windows.FILE_SHARE_READ)
		if isDir {
			access, share = windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE
		}
		handle, err := windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return nil, windowsHandoffError("path_invalid", err)
		}
		handles = append(handles, handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != isDir {
			return nil, windowsHandoffError("path_invalid", err)
		}
	}
	return cleanup, nil
}
