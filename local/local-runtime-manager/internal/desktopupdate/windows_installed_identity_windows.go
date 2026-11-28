package desktopupdate

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readInstalledFileIdentity(file *os.File, _ os.FileInfo) (installedFileIdentity, error) {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return installedFileIdentity{}, err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return installedFileIdentity{}, errors.New("installed file has a link or reparse point")
	}
	var identity installedFileIdentity
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&identity)), uint32(unsafe.Sizeof(identity))); err != nil {
		return installedFileIdentity{}, err
	}
	if identity.ID == [16]byte{} {
		return installedFileIdentity{}, errors.New("installed file identity is unavailable")
	}
	return identity, nil
}
