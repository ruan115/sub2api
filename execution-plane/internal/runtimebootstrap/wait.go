package runtimebootstrap

import (
	"context"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

// Wait initializes the local identity and CSR before waiting, not after runtime
// readiness. No listener exists until the installed mTLS identity verifies.
func Wait(ctx context.Context, c Config) error {
	if ctx == nil || ctx.Err() != nil || c.Validate() != nil {
		return ErrBootstrap
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ErrBootstrap
		}
		err := withParent(c, true, func(root *os.Root, dir *os.File) error {
			identity, err := runtimeidentity.Open(c.IdentityDirectory, c.Binding, true)
			if err != nil {
				return ErrBootstrap
			}
			trust, err := readPublic(root, "runtime-ca.pem")
			if err != nil {
				return err
			}
			if !matchesTrustPin(c, trust) {
				return ErrBootstrap
			}
			if !identity.HasCertificate() {
				return ErrNotReady
			}
			if _, err := runtimeidentity.LoadServerTLS(c.IdentityDirectory, c.Binding, trust); err != nil {
				return ErrBootstrap
			}
			return nil
		})
		if err != ErrNotReady {
			if ctx.Err() != nil {
				return ErrBootstrap
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ErrBootstrap
		case <-ticker.C:
		}
	}
}
