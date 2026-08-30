package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProtectedRegularFileRequiresOwnerReadAndRejectsSharedWrites(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "private.key")
	writePKITestFile(t, privatePath, []byte("private"), 0o200)
	if _, err := readProtectedRegularFile(privatePath, 1024, true); err != errProtectedFile {
		t.Fatalf("owner-write-only private file error = %v", err)
	}
	if err := os.Chmod(privatePath, 0o400); err != nil {
		t.Fatal(err)
	}
	if payload, err := readProtectedRegularFile(privatePath, 1024, true); err != nil || string(payload) != "private" {
		t.Fatalf("owner-readable private file = %q, %v", payload, err)
	}

	certificatePath := filepath.Join(directory, "server.crt")
	writePKITestFile(t, certificatePath, []byte("certificate"), 0o664)
	if err := os.Chmod(certificatePath, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedRegularFile(certificatePath, 1024, false); err != errProtectedFile {
		t.Fatalf("group-writable certificate error = %v", err)
	}
	if err := os.Chmod(certificatePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if payload, err := readProtectedRegularFile(certificatePath, 1024, false); err != nil || string(payload) != "certificate" {
		t.Fatalf("read-only certificate = %q, %v", payload, err)
	}
}
