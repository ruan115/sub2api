package runtimebootstrap

import (
	"os"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

// RequestIdentity reads the key prepared by normal worker startup and produces
// a public CSR. Issuance idempotency binds SPKI, not random CSR signature bytes.
// An exec helper cannot initialize a second key or replace old process state.
func RequestIdentity(c Config) (Request, error) {
	var result Request
	err := withParent(c, false, func(root *os.Root, _ *os.File) error {
		if _, err := root.Lstat("identity/instance-identity.json"); os.IsNotExist(err) {
			return ErrNotReady
		} else if err != nil {
			return ErrBootstrap
		}
		identity, err := runtimeidentity.Open(c.IdentityDirectory, c.Binding, false)
		if err != nil {
			return ErrBootstrap
		}
		csr, err := identity.CSR()
		if err != nil {
			return ErrBootstrap
		}
		result = Request{Binding: c.Binding, CSRPEM: csr}
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return result, nil
}
