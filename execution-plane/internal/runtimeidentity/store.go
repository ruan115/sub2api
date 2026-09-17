package runtimeidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

const stateName = "instance-identity.json"
const maxStateBytes = 16 * 1024

// Identity deliberately has no exported private-key field. Public() and CSR()
// are the only export paths; JSON formatting of Identity cannot reveal a key.
type Identity struct {
	binding   Binding
	machineID string
	key       *ecdsa.PrivateKey
}

type Public struct {
	Binding         Binding `json:"binding"`
	MachineID       string  `json:"machine_id"`
	PublicKeySHA256 string  `json:"public_key_sha256"`
}

type diskState struct {
	Version    int     `json:"version"`
	Binding    Binding `json:"binding"`
	MachineID  string  `json:"machine_id"`
	PrivateKey []byte  `json:"private_key_pkcs8"`
}

func (i *Identity) Public() Public {
	der, _ := x509.MarshalPKIXPublicKey(&i.key.PublicKey)
	hash := sha256.Sum256(der)
	return Public{i.binding, i.machineID, hex.EncodeToString(hash[:])}
}

func (i *Identity) CSR() ([]byte, error) {
	u, err := i.binding.URI()
	if err != nil {
		return nil, ErrIdentity
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}}, i.key)
	if err != nil {
		return nil, ErrIdentity
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Open initializes only when create is true and no state exists. Any existing
// invalid or differently bound state fails closed, never silently replaces it.
// The parent must be an already provisioned, owner-only instance directory.
// A trusted host/kernel and no hostile concurrent same-UID writer are assumed.
func Open(directory string, binding Binding, create bool) (*Identity, error) {
	if binding.Validate() != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, ErrIdentity
	}
	physical, err := filepath.EvalSymlinks(directory)
	if err != nil || physical != directory {
		return nil, ErrIdentity
	}
	before, err := os.Lstat(directory)
	if err != nil || !safeInfo(before, true) {
		return nil, ErrIdentity
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, ErrIdentity
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, ErrIdentity
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !safeInfo(opened, true) || !os.SameFile(before, opened) {
		return nil, ErrIdentity
	}
	lock, err := root.OpenFile("instance-identity.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, ErrIdentity
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !safeInfo(info, false) || info.Size() != 0 || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, ErrIdentity
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// This is a dedicated identity directory, not the CLI's entire home. A
	// crashed initializer's temporary secret must trigger operator recovery,
	// never implicit replacement with another identity or automatic deletion.
	names, scanErr := dir.Readdirnames(3)
	if scanErr != nil && scanErr != io.EOF {
		return nil, ErrIdentity
	}
	for _, name := range names {
		if name != stateName && name != "instance-identity.lock" {
			return nil, ErrIdentity
		}
	}
	_, err = root.Lstat(stateName)
	if os.IsNotExist(err) && create {
		if err = initialize(root, dir, binding); err != nil {
			return nil, ErrIdentity
		}
	} else if err != nil {
		return nil, ErrIdentity
	}
	identity, err := read(root, binding)
	after, statErr := os.Lstat(directory)
	if err != nil || statErr != nil || !safeInfo(after, true) || !os.SameFile(opened, after) {
		return nil, ErrIdentity
	}
	return identity, nil
}

func safeInfo(info os.FileInfo, directory bool) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600 && info.Mode().IsRegular() && stat.Nlink == 1
}

func initialize(root *os.Root, dir *os.File, binding Binding) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ErrIdentity
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return ErrIdentity
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return ErrIdentity
	}
	defer clear(der)
	data, err := json.Marshal(diskState{1, binding, hex.EncodeToString(id), der})
	if err != nil {
		return ErrIdentity
	}
	defer clear(data)
	name := ".identity-init-" + hex.EncodeToString(id)
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrIdentity
	}
	defer root.Remove(name) // only this invocation's random temporary file
	if n, err := file.Write(data); err != nil || n != len(data) {
		file.Close()
		return ErrIdentity
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return ErrIdentity
	}
	if err := file.Close(); err != nil {
		return ErrIdentity
	}
	// Link is no-replace. Unlike Rename it cannot overwrite an existing state.
	if err := root.Link(name, stateName); err != nil {
		return ErrIdentity
	}
	if err := root.Remove(name); err != nil {
		return ErrIdentity
	}
	if err := dir.Sync(); err != nil {
		return ErrIdentity
	}
	return nil
}

func read(root *os.Root, binding Binding) (*Identity, error) {
	before, err := root.Lstat(stateName)
	if err != nil || !safeInfo(before, false) || before.Size() <= 0 || before.Size() > maxStateBytes {
		return nil, ErrIdentity
	}
	file, err := root.OpenFile(stateName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIdentity
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !safeInfo(info, false) || !os.SameFile(before, info) {
		return nil, ErrIdentity
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	defer clear(data)
	if err != nil || len(data) == 0 || len(data) > maxStateBytes {
		return nil, ErrIdentity
	}
	after, err := root.Lstat(stateName)
	if err != nil || !safeInfo(after, false) || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil, ErrIdentity
	}
	var state diskState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, ErrIdentity
	}
	defer clear(state.PrivateKey)
	// Require our exact encoding, rejecting duplicate fields, trailing bytes,
	// alternate spellings and partially published state without custom parsers.
	canonical, err := json.Marshal(state)
	defer clear(canonical)
	if err != nil || !bytes.Equal(canonical, data) || state.Version != 1 || state.Binding != binding || !accountPattern.MatchString(state.MachineID) {
		return nil, ErrIdentity
	}
	parsed, err := x509.ParsePKCS8PrivateKey(state.PrivateKey)
	if err != nil {
		return nil, ErrIdentity
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, ErrIdentity
	}
	return &Identity{binding, state.MachineID, key}, nil
}
