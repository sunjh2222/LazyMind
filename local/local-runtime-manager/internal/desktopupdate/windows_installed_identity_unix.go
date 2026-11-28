//go:build !windows

package desktopupdate

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
)

func readInstalledFileIdentity(_ *os.File, info os.FileInfo) (installedFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 || stat.Nlink != 1 {
		return installedFileIdentity{}, errors.New("installed file has links or its identity is unavailable")
	}
	identity := installedFileIdentity{Volume: uint64(stat.Dev)}
	binary.LittleEndian.PutUint64(identity.ID[:], stat.Ino)
	return identity, nil
}
