package systemdeps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const selectedComponentContentMarker = ".selected-content-sha256"

func writeSelectedComponentContent(dir string, entry PythonComponent) error {
	digest, err := selectedComponentContentDigest(dir, entry.UnpackedBytes)
	if err != nil {
		return err
	}
	return componentAtomicWrite(filepath.Join(dir, selectedComponentContentMarker), []byte(entry.SHA256+"\n"+digest+"\n"))
}

func selectedComponentContentMatches(dir string, entry PythonComponent) bool {
	raw, err := componentFileBytes(filepath.Join(dir, selectedComponentContentMarker), 130)
	if err != nil || len(raw) != 130 || string(raw[:65]) != entry.SHA256+"\n" {
		return false
	}
	digest, err := selectedComponentContentDigest(dir, entry.UnpackedBytes)
	return err == nil && string(raw[65:]) == digest+"\n"
}

func selectedComponentContentMissing(root string, entry PythonComponent) bool {
	dir := root
	for _, part := range []string{"deps", "python-components", entry.ID, entry.Revision, "site-packages"} {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if err != nil || info.Mode().Type() != os.ModeDir {
			return false
		}
	}
	_, err := os.Lstat(filepath.Join(filepath.Dir(dir), selectedComponentContentMarker))
	return os.IsNotExist(err)
}

// Only payload files participate. Python can create bytecode caches after the
// verified ZIP is extracted. Reading a damaged overlay must never import it.
func selectedComponentContentDigest(dir string, budget int64) (string, error) {
	invalid := errors.New("component_file_invalid")
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Type() != os.ModeDir {
		return "", invalid
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	digest := sha256.New()
	err = fs.WalkDir(root.FS(), "site-packages", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Type() != os.ModeDir {
				return invalid
			}
			if entry.Name() == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > budget {
			return invalid
		}
		file, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
			return invalid
		}
		fileHash := sha256.New()
		size, err := io.Copy(fileHash, io.LimitReader(file, info.Size()+1))
		if err != nil || size != info.Size() {
			return invalid
		}
		budget -= size
		_, _ = io.WriteString(digest, name+"\x00")
		_, _ = digest.Write(fileHash.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
