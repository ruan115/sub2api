package runtimeidentity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func testBinding() Binding { return Binding{strings.Repeat("a", 32), "slot-a", "node-a", 7, 3} }
func privateDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLocalIdentityIndependentAndRestartStable(t *testing.T) {
	a, b := privateDir(t), privateDir(t)
	binding := testBinding()
	first, err := Open(a, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(a, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(b, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Public() != again.Public() || first.Public().MachineID == other.Public().MachineID || first.Public().PublicKeySHA256 == other.Public().PublicKeySHA256 {
		t.Fatal("identity isolation or restart violated")
	}
	for _, instance := range []*Identity{first, other} {
		csr, err := instance.CSR()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateCSR(binding, csr); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(instance)
		if err != nil || string(encoded) != "{}" {
			t.Fatal("identity serialization is not private")
		}
	}
	info, err := os.Stat(filepath.Join(a, stateName))
	if err != nil || !safeInfo(info, false) {
		t.Fatal("private file permissions")
	}
}

func TestWrongBindingNeverMutatesExistingState(t *testing.T) {
	dir := privateDir(t)
	binding := testBinding()
	if _, err := Open(dir, binding, true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, stateName))
	for field := 0; field < 5; field++ {
		wrong := binding
		switch field {
		case 0:
			wrong.AccountHash = strings.Repeat("b", 32)
		case 1:
			wrong.SlotID = "slot-b"
		case 2:
			wrong.NodeID = "node-b"
		case 3:
			wrong.Epoch++
		case 4:
			wrong.Generation++
		}
		if _, err := Open(dir, wrong, true); err != ErrIdentity {
			t.Fatal("mismatched binding accepted")
		}
		after, _ := os.ReadFile(filepath.Join(dir, stateName))
		if !bytes.Equal(before, after) {
			t.Fatal("mismatched state overwritten")
		}
	}
}

func TestIdentityRejectsUnsafeStateWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "mode", "empty", "large", "broken", "duplicate", "unknown", "key", "version", "lock-symlink", "lock-hardlink", "lock-fifo", "lock-content"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateDir(t)
			binding := testBinding()
			if _, err := Open(dir, binding, true); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, stateName)
			data, _ := os.ReadFile(path)
			switch kind {
			case "symlink":
				outside := filepath.Join(privateDir(t), "original")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(privateDir(t), "linked")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				os.Remove(path)
				syscall.Mkfifo(path, 0600)
			case "directory":
				os.Remove(path)
				os.Mkdir(path, 0700)
			case "mode":
				os.Chmod(path, 0640)
			case "empty":
				os.WriteFile(path, nil, 0600)
			case "large":
				os.WriteFile(path, bytes.Repeat([]byte("x"), maxStateBytes+1), 0600)
			case "broken":
				os.WriteFile(path, []byte("PRIVATE_TEST_NOT_FOR_ERROR"), 0600)
			case "duplicate":
				os.WriteFile(path, append([]byte(`{"version":1,`), data[1:]...), 0600)
			case "unknown":
				os.WriteFile(path, append([]byte(`{"unknown":1,`), data[1:]...), 0600)
			case "key", "version":
				var state diskState
				json.Unmarshal(data, &state)
				if kind == "key" {
					state.PrivateKey = []byte("bad key")
				} else {
					state.Version++
				}
				changed, _ := json.Marshal(state)
				os.WriteFile(path, changed, 0600)
			case "lock-symlink", "lock-hardlink", "lock-fifo", "lock-content":
				lock := filepath.Join(dir, "instance-identity.lock")
				if kind == "lock-hardlink" {
					os.Link(lock, lock+".link")
				} else if kind == "lock-content" {
					os.WriteFile(lock, []byte("unexpected"), 0600)
				} else {
					os.Remove(lock)
					if kind == "lock-fifo" {
						syscall.Mkfifo(lock, 0600)
					} else {
						os.Symlink(path, lock)
					}
				}
			}
			if _, err := Open(dir, binding, true); err != ErrIdentity {
				t.Fatal("unsafe state accepted")
			}
		})
	}
}

func TestIdentityRejectsUnsafeDirectoryAndMissingRead(t *testing.T) {
	dir := privateDir(t)
	if _, err := Open(dir, testBinding(), false); err != ErrIdentity {
		t.Fatal("show created identity")
	}
	if _, err := os.Stat(filepath.Join(dir, stateName)); !os.IsNotExist(err) {
		t.Fatal("state created")
	}
	os.Chmod(dir, 0755)
	if _, err := Open(dir, testBinding(), true); err != ErrIdentity {
		t.Fatal("public directory accepted")
	}
	os.Chmod(dir, 0700)
	alias := filepath.Join(privateDir(t), "link")
	os.Symlink(dir, alias)
	for _, path := range []string{alias, "relative", dir + "/../" + filepath.Base(dir)} {
		if _, err := Open(path, testBinding(), true); err != ErrIdentity {
			t.Fatal("unsafe directory accepted")
		}
	}
}

func TestConcurrentInitializationNeverReturnsDifferentKeys(t *testing.T) {
	dir := privateDir(t)
	var wg sync.WaitGroup
	values := make(chan Public, 12)
	for range 12 {
		wg.Go(func() {
			if id, err := Open(dir, testBinding(), true); err == nil {
				values <- id.Public()
			} else if err != ErrIdentity {
				t.Error("unexpected error")
			}
		})
	}
	wg.Wait()
	close(values)
	final, err := Open(dir, testBinding(), false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for v := range values {
		count++
		if v != final.Public() {
			t.Fatal("competing identity published")
		}
	}
	if count == 0 {
		t.Fatal("no initializer succeeded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatal("unexpected state or temporary residue")
	}
}

func TestPartialInitializationRejectsWithoutReplacingOrDeleting(t *testing.T) {
	dir := privateDir(t)
	partial := filepath.Join(dir, ".identity-init-interrupted")
	original := []byte("synthetic-incomplete-private-state")
	if err := os.WriteFile(partial, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, testBinding(), true); err != ErrIdentity {
		t.Fatal("interrupted init accepted")
	}
	after, err := os.ReadFile(partial)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("interrupted state modified")
	}
	if _, err := os.Stat(filepath.Join(dir, stateName)); !os.IsNotExist(err) {
		t.Fatal("replacement identity created")
	}
}
