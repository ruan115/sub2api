package pki

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/url"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrRuntimeCertificate = errors.New("runtime certificate request rejected")

// IssueRuntime signs only the public key in an exact, self-signed runtime CSR.
// The caller must authenticate and authorize binding before calling this
// library method. It does not authorize enrollment or install TLS credentials.
// The instance's private key is never generated or returned by this authority.
func (a *Authority) IssueRuntime(binding runtimeidentity.Binding, csrPEM []byte) (IssuedCertificate, error) {
	if a == nil || a.certificate == nil || a.signer == nil || a.now == nil || a.certificateTTL <= 0 {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	request, err := runtimeidentity.ValidateCSR(binding, csrPEM)
	if err != nil {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	identity, err := binding.URI()
	if err != nil {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(request.PublicKey)
	if err != nil {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	current := a.now().UTC()
	if current.Before(a.certificate.NotBefore) || !current.Before(a.certificate.NotAfter) {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	// Validate the CA and issue against one clock observation. In particular, a
	// later clock rollback must not bypass the CA NotBefore check above.
	issuer := *a
	issuer.now = func() time.Time { return current }
	issued, err := issuer.issue(pkix.Name{}, request.PublicKey, publicKeyDER, nil,
		[]*url.URL{identity}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return IssuedCertificate{}, ErrRuntimeCertificate
	}
	return issued, nil
}
