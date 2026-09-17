package daemon

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"golang.org/x/sys/unix"
)

var ErrIdentity = errors.New("host runtime identity rejected")

type Identity struct {
	TrustPEM        []byte
	NodeCertificate tls.Certificate
	ControlTLS      *tls.Config
	ExpiresAt       time.Time
}

func (Identity) String() string               { return "host runtime identity [redacted]" }
func (i Identity) GoString() string           { return i.String() }
func (i Identity) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, i.String()) }
func (Identity) MarshalJSON() ([]byte, error) { return []byte(`{"identity":"redacted"}`), nil }

// LoadIdentity loads a pre-issued node identity; it never creates a CA, signs
// anything or enrolls a node. All three inputs remain fixed during validation.
func LoadIdentity(nodeID string, cfg Config) (*Identity, error) {
	if !cfg.Enabled || cfg.Validate() != nil {
		return nil, ErrIdentity
	}
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("0", 32), SlotID: "validation", NodeID: nodeID, Epoch: 1, Generation: 1}
	if binding.Validate() != nil {
		return nil, ErrIdentity
	}
	var inputs []*identityInput
	defer func() {
		for _, input := range inputs {
			input.close()
		}
	}()
	for index, path := range []string{cfg.TrustFile, cfg.NodeCertFile, cfg.NodeKeyFile} {
		input, err := readIdentityInput(path, index == 2)
		if err != nil {
			return nil, ErrIdentity
		}
		inputs = append(inputs, input)
		for _, earlier := range inputs[:len(inputs)-1] {
			if os.SameFile(earlier.info, input.info) {
				return nil, ErrIdentity
			}
		}
	}
	certBlock, err := exactPEM(inputs[1].data, "CERTIFICATE")
	if err != nil {
		return nil, ErrIdentity
	}
	keyBlock, err := exactPEM(inputs[2].data, "PRIVATE KEY", "EC PRIVATE KEY")
	if err != nil {
		return nil, ErrIdentity
	}
	defer wipe(keyBlock.Bytes)
	var privateKey any
	if keyBlock.Type == "PRIVATE KEY" {
		privateKey, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	} else {
		privateKey, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	}
	if err != nil {
		return nil, ErrIdentity
	}
	validated, err := runtimeidentity.ClientTLS(binding, inputs[0].data, tls.Certificate{Certificate: [][]byte{certBlock.Bytes}, PrivateKey: privateKey})
	if err != nil {
		return nil, ErrIdentity
	}
	rootBlock, err := exactPEM(inputs[0].data, "CERTIFICATE")
	if err != nil {
		return nil, ErrIdentity
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		return nil, ErrIdentity
	}
	certificate := validated.Certificates[0]
	expires := certificate.Leaf.NotAfter
	if root.NotAfter.Before(expires) {
		expires = root.NotAfter
	}
	for _, input := range inputs {
		if input.verify() != nil {
			return nil, ErrIdentity
		}
	}
	if !time.Now().Before(expires) {
		return nil, ErrIdentity
	}
	// Deliberately build a separate control config: the runtime VerifyConnection
	// closure is exact-instance-specific and is not valid for an orchestrator.
	control := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: cfg.ControlServerName,
		RootCAs: validated.RootCAs.Clone(), Certificates: []tls.Certificate{certificate}}
	return &Identity{TrustPEM: bytes.Clone(inputs[0].data), NodeCertificate: certificate, ControlTLS: control, ExpiresAt: expires}, nil
}

func exactPEM(data []byte, types ...string) (*pem.Block, error) {
	trimmed := bytes.TrimSpace(data)
	block, rest := pem.Decode(trimmed)
	if block == nil || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || !bytes.HasPrefix(trimmed, []byte("-----BEGIN "+block.Type+"-----\n")) {
		return nil, ErrIdentity
	}
	for _, kind := range types {
		if block.Type == kind {
			return block, nil
		}
	}
	return nil, ErrIdentity
}

type identityInput struct {
	path    string
	private bool
	file    *os.File
	info    os.FileInfo
	data    []byte
}

func wipe(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
func (input *identityInput) close() { wipe(input.data); _ = input.file.Close() }

func safeIdentityFile(info os.FileInfo, private bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 != 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeType|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		return false
	}
	if private && (stat.Uid != uint32(os.Geteuid()) || (info.Mode().Perm() != 0400 && info.Mode().Perm() != 0600)) {
		return false
	}
	limit := int64(16 << 10)
	if private {
		limit = 8 << 10
	}
	return info.Size() > 0 && info.Size() <= limit
}

// Walk from a pinned root with O_NOFOLLOW for every component. World-writable
// sticky root-owned ancestors (e.g. /tmp) may be traversed, never used as the
// immediate identity directory. No FIFO/device can block the bounded reader.
func openIdentityFile(path string, private bool) (*os.File, os.FileInfo, error) {
	if !cleanPath(path) {
		return nil, nil, ErrIdentity
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 128 {
		return nil, nil, ErrIdentity
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, ErrIdentity
	}
	parent := os.NewFile(uintptr(fd), "identity-parent")
	defer func() { _ = parent.Close() }()
	for index, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil, ErrIdentity
		}
		_ = parent.Close()
		parent = os.NewFile(uintptr(next), "identity-parent")
		info, err := parent.Stat()
		if err != nil {
			return nil, nil, ErrIdentity
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return nil, nil, ErrIdentity
		}
		if info.Mode().Perm()&0022 != 0 && (index == len(parts)-2 || info.Mode()&os.ModeSticky == 0 || stat.Uid != 0) {
			return nil, nil, ErrIdentity
		}
	}
	fd, err = unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, ErrIdentity
	}
	file := os.NewFile(uintptr(fd), "identity-input")
	info, err := file.Stat()
	if err != nil || !safeIdentityFile(info, private) {
		_ = file.Close()
		return nil, nil, ErrIdentity
	}
	return file, info, nil
}

func readIdentityInput(path string, private bool) (*identityInput, error) {
	file, info, err := openIdentityFile(path, private)
	if err != nil {
		return nil, ErrIdentity
	}
	input := &identityInput{path: path, private: private, file: file, info: info}
	input.data, err = io.ReadAll(io.LimitReader(file, info.Size()+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(input.data)) != info.Size() || !sameIdentityFile(info, after, private) {
		input.close()
		return nil, ErrIdentity
	}
	return input, nil
}

func sameIdentityFile(a, b os.FileInfo, private bool) bool {
	return safeIdentityFile(b, private) && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (input *identityInput) verify() error {
	after, err := input.file.Stat()
	if err != nil || !sameIdentityFile(input.info, after, input.private) {
		return ErrIdentity
	}
	current, err := readIdentityInput(input.path, input.private)
	if err != nil {
		return ErrIdentity
	}
	defer current.close()
	if !sameIdentityFile(input.info, current.info, input.private) || !bytes.Equal(input.data, current.data) {
		return ErrIdentity
	}
	return nil
}
