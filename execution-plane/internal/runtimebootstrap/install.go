package runtimebootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func matchesTrustPin(c Config, ca []byte) bool {
	digest := sha256.Sum256(ca)
	return len(ca) > 0 && len(ca) <= 16*1024 && hex.EncodeToString(digest[:]) == c.TrustSHA256
}

// Install accepts only the pinned public CA and this instance's first matching
// leaf. It never imports a key, rotates a certificate or resets corrupt state.
func Install(c Config, bundle PublicBundle) error {
	if c.Validate() != nil || !matchesTrustPin(c, bundle.CAPEM) || len(bundle.CertificatePEM) == 0 || len(bundle.CertificatePEM) > 16*1024 {
		return ErrBootstrap
	}
	return withParent(c, false, func(root *os.Root, dir *os.File) error {
		identity, err := runtimeidentity.Open(c.IdentityDirectory, c.Binding, false)
		if err != nil {
			return ErrBootstrap
		}
		if _, err := identity.ServerTLS(bundle.CertificatePEM, bundle.CAPEM); err != nil {
			return ErrBootstrap
		}
		if err := publishPublic(root, dir, "runtime-ca.pem", bundle.CAPEM); err != nil {
			return err
		}
		if err := runtimeidentity.InstallCertificate(c.IdentityDirectory, c.Binding, bundle.CertificatePEM, bundle.CAPEM); err != nil {
			return ErrBootstrap
		}
		return nil
	})
}
