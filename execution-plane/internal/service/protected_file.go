package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

var errProtectedFile = errors.New("protected file rejected")

func readProtectedRegularFile(path string, maxBytes int64, ownerOnly bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maxBytes <= 0 {
		return nil, errProtectedFile
	}
	info, err := os.Lstat(path)
	if err != nil || !validProtectedFileInfo(info, maxBytes, ownerOnly) || info.Mode()&os.ModeSymlink != 0 {
		return nil, errProtectedFile
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errProtectedFile
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !validProtectedFileInfo(openedInfo, maxBytes, ownerOnly) {
		return nil, errProtectedFile
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(payload) == 0 || int64(len(payload)) > maxBytes {
		eraseLoaderBytes(payload)
		return nil, errProtectedFile
	}
	return payload, nil
}

func validProtectedFileInfo(info os.FileInfo, maxBytes int64, ownerOnly bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBytes {
		return false
	}
	if ownerOnly {
		return info.Mode().Perm()&0o400 != 0 && info.Mode().Perm()&0o077 == 0
	}
	return info.Mode().Perm()&0o400 != 0 && info.Mode().Perm()&0o022 == 0
}
