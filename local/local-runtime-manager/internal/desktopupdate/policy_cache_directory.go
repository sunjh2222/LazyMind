package desktopupdate

import (
	"errors"
	"os"
	"path/filepath"
)

// ValidateWindowsPolicyDownloadCache checks the cache directory chain rooted
// at the updater's selected data directory. Missing directories can be created
// by the downloader; existing path components may not redirect the cache.
func ValidateWindowsPolicyDownloadCache(root string, c Config) error {
	if c.Mode != "windows-policy" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return failure("path_invalid", nil)
	}
	cache := DownloadCacheDirectory(root, c)
	for _, dir := range []string{root, filepath.Dir(cache), cache} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return failure("cache_read", err)
		}
		if info.Mode().Type() != os.ModeDir || policyCacheDirectoryIsReparsePoint(info) {
			return failure("path_invalid", nil)
		}
	}
	return nil
}
