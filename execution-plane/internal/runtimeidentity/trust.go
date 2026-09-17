package runtimeidentity

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ReadTrustFile loads public trust material from an explicitly provisioned path.
// It does not discover trust from the peer, ambient environment, or OS CA pool.
// Administrators/current UID are trusted; writable shared ancestors are refused
// except root-owned sticky temporary directories. Same-UID attackers are outside
// this filesystem contract, as for the instance identity store.
func ReadTrustFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrIdentity
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return nil, ErrIdentity
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() {
			return nil, ErrIdentity
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return nil, ErrIdentity
		}
		if info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return nil, ErrIdentity
		}
		if parent == "/" {
			break
		}
	}
	safe := func(info os.FileInfo) bool {
		if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() <= 0 || info.Size() > 16*1024 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && stat.Nlink == 1 && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid()))
	}
	before, err := os.Lstat(path)
	if err != nil || !safe(before) {
		return nil, ErrIdentity
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIdentity
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !safe(info) || !os.SameFile(before, info) {
		return nil, ErrIdentity
	}
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	after, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !safe(after) || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || len(data) == 0 || len(data) > 16*1024 {
		return nil, ErrIdentity
	}
	return data, nil
}
