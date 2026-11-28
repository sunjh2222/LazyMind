//go:build !windows

package desktopupdate

import "os"

func policyCacheDirectoryIsReparsePoint(os.FileInfo) bool { return false }
