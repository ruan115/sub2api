package runtimeidentity

import (
	"crypto/x509"
	"time"
)

// ValidateNodeCertificate applies the same strict node identity policy as
// runtime mTLS without requiring or accepting a node private key.
func ValidateNodeCertificate(nodeID string, leaf *x509.Certificate, trustPEM []byte, now time.Time) error {
	_, roots, err := trustRoot(trustPEM, now)
	if err != nil || verifyNode(nodeID, leaf, roots, now) != nil {
		return ErrIdentity
	}
	return nil
}
