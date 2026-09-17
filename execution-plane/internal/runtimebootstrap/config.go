// Package runtimebootstrap owns the pre-listener, instance-local enrollment
// mailbox. It never contains an issuer, a network listener or a private-key
// export path. The host transports only a CSR and public certificates.
package runtimebootstrap

import (
	"errors"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrBootstrap = errors.New("runtime bootstrap rejected")
var ErrNotReady = errors.New("runtime bootstrap not ready")

const DefaultTimeout = 45 * time.Second
const MaxPublicBytes = 24 * 1024
const MaxEncodedBundleBytes = 32 * 1024

var pinPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Config struct {
	IdentityDirectory string
	TrustFile         string
	TrustSHA256       string
	Binding           runtimeidentity.Binding
	Timeout           time.Duration
}

type Request struct {
	Binding runtimeidentity.Binding `json:"binding"`
	CSRPEM  []byte                  `json:"csr_pem"`
}

type PublicBundle struct {
	CertificatePEM []byte `json:"certificate_pem"`
	CAPEM          []byte `json:"ca_pem"`
}

func ValidTrustPin(value string) bool { return pinPattern.MatchString(value) }

func (c Config) Validate() error {
	if c.Binding.Validate() != nil || !ValidTrustPin(c.TrustSHA256) ||
		!filepath.IsAbs(c.IdentityDirectory) || filepath.Clean(c.IdentityDirectory) != c.IdentityDirectory ||
		filepath.Base(c.IdentityDirectory) != "identity" ||
		c.TrustFile != filepath.Join(filepath.Dir(c.IdentityDirectory), "runtime-ca.pem") ||
		c.Timeout < 0 || c.Timeout > DefaultTimeout {
		return ErrBootstrap
	}
	return nil
}
