package runtimebootstrap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// The host/current UID are trusted. Root-owned sticky temporary directories
// can contain a private parent; no shared-writable non-sticky ancestor can.
func trustedAncestors(path string) error {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return ErrBootstrap
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() {
			return ErrBootstrap
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) ||
			(info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)) {
			return ErrBootstrap
		}
		if current == "/" {
			return nil
		}
	}
}

func privateInfo(info os.FileInfo, directory bool) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600 && info.Mode().IsRegular() && stat.Nlink == 1
}

func withParent(c Config, create bool, fn func(*os.Root, *os.File) error) error {
	if c.Validate() != nil || os.Geteuid() == 0 {
		return ErrBootstrap
	}
	parent := filepath.Dir(c.IdentityDirectory)
	if create {
		if trustedAncestors(filepath.Dir(parent)) != nil {
			return ErrBootstrap
		}
		if err := os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
			return ErrBootstrap
		}
	}
	before, err := os.Lstat(parent)
	if os.IsNotExist(err) && !create {
		return ErrNotReady
	}
	if err != nil || !privateInfo(before, true) || trustedAncestors(parent) != nil {
		return ErrBootstrap
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return ErrBootstrap
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return ErrBootstrap
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !privateInfo(opened, true) || !os.SameFile(before, opened) {
		return ErrBootstrap
	}
	lock, err := root.OpenFile("bootstrap.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return ErrBootstrap
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !privateInfo(info, false) || info.Size() != 0 {
		return ErrBootstrap
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return ErrNotReady
		}
		return ErrBootstrap
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	names, err := dir.Readdirnames(4)
	if err != nil && err != io.EOF {
		return ErrBootstrap
	}
	for _, name := range names {
		if name != "identity" && name != "runtime-ca.pem" && name != "bootstrap.lock" {
			return ErrBootstrap
		}
	}
	if create {
		if err := root.Mkdir("identity", 0700); err != nil && !os.IsExist(err) {
			return ErrBootstrap
		}
	}
	identityInfo, err := root.Lstat("identity")
	if os.IsNotExist(err) && !create {
		return ErrNotReady
	}
	if err != nil || !privateInfo(identityInfo, true) {
		return ErrBootstrap
	}
	err = fn(root, dir)
	after, statErr := os.Lstat(parent)
	if statErr != nil || !privateInfo(after, true) || !os.SameFile(opened, after) {
		return ErrBootstrap
	}
	return err
}

func readPublic(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, ErrNotReady
	}
	if err != nil || !privateInfo(before, false) || before.Size() <= 0 || before.Size() > MaxPublicBytes {
		return nil, ErrBootstrap
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrBootstrap
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !privateInfo(opened, false) || !os.SameFile(before, opened) {
		return nil, ErrBootstrap
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxPublicBytes+1))
	after, afterErr := root.Lstat(name)
	if err != nil || afterErr != nil || !privateInfo(after, false) || !os.SameFile(opened, after) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || len(data) == 0 || len(data) > MaxPublicBytes {
		return nil, ErrBootstrap
	}
	return data, nil
}

func publishPublic(root *os.Root, dir *os.File, name string, data []byte) error {
	if len(data) == 0 || len(data) > MaxPublicBytes {
		return ErrBootstrap
	}
	existing, err := readPublic(root, name)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return ErrBootstrap
		}
		return nil
	}
	if err != ErrNotReady {
		return ErrBootstrap
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrBootstrap
	}
	temporary := ".bootstrap-publish-" + hex.EncodeToString(random[:])
	f, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrBootstrap
	}
	defer root.Remove(temporary)
	if n, err := f.Write(data); err != nil || n != len(data) {
		f.Close()
		return ErrBootstrap
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return ErrBootstrap
	}
	if err := f.Close(); err != nil {
		return ErrBootstrap
	}
	if err := root.Link(temporary, name); err != nil {
		return ErrBootstrap
	}
	if err := root.Remove(temporary); err != nil {
		return ErrBootstrap
	}
	if err := dir.Sync(); err != nil {
		return ErrBootstrap
	}
	return nil
}
