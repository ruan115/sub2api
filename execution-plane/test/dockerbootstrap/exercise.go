package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type liveEngine interface {
	inspector
	Ping(context.Context) error
	BootstrapRequestExec(context.Context, string, uint32) ([]byte, error)
	BootstrapInstallExec(context.Context, string, uint32, runtimebootstrap.PublicBundle) error
	ExecContainer(context.Context, string, []string) (docker.ExecResult, error)
}

type experiment struct {
	config initialInput
	public publicConfig
	ids    []string
	engine liveEngine
	world  *controlWorld
	stage  string // fixed internal phase; never derived from requests or errors
}

func parseLeaf(value []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(value)
	if len(value) > 16<<10 || block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, rejected
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, rejected
	}
	return leaf, nil
}

func (e *experiment) checked(ctx context.Context, index int, operation func() error) error {
	if inspect(ctx, e.engine, e.config, e.public, e.ids, index) != nil {
		return rejected
	}
	err := operation()
	if inspect(ctx, e.engine, e.config, e.public, e.ids, index) != nil || ctx.Err() != nil {
		return rejected
	}
	return err
}

func (e *experiment) request(ctx context.Context, index int) (runtimebootstrap.Request, [32]byte, error) {
	var data []byte
	err := until(ctx, func() (bool, error) {
		err := e.checked(ctx, index, func() error {
			var err error
			data, err = e.engine.BootstrapRequestExec(ctx, e.ids[index], 1000)
			return err
		})
		if errors.Is(err, runtimebootstrap.ErrNotReady) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return runtimebootstrap.Request{}, [32]byte{}, rejected
	}
	r, err := runtimebootstrap.DecodeRequest(data)
	if err != nil || r.Binding != binding(index) {
		return runtimebootstrap.Request{}, [32]byte{}, rejected
	}
	csr, err := runtimeidentity.ValidateCSR(r.Binding, r.CSRPEM)
	if err != nil {
		return runtimebootstrap.Request{}, [32]byte{}, rejected
	}
	spki, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return runtimebootstrap.Request{}, [32]byte{}, rejected
	}
	return r, sha256.Sum256(spki), nil
}

func (e *experiment) enroll(ctx context.Context, r runtimebootstrap.Request) (runtimebootstrap.PublicBundle, error) {
	b := r.Binding
	response, err := e.world.client.EnrollRuntimeCertificate(ctx, &executionv1.EnrollRuntimeCertificateRequest{SlotId: b.SlotID, ExecutionEpoch: b.Epoch, RuntimeGeneration: b.Generation, CsrPem: r.CSRPEM})
	if err != nil {
		return runtimebootstrap.PublicBundle{}, err
	}
	trust := e.world.authority.CertificatePEM()
	leaf, err := runtimeidentity.ValidateCertificate(b, response.GetCertificatePem(), trust, time.Now())
	csr, csrErr := runtimeidentity.ValidateCSR(b, r.CSRPEM)
	if err != nil || csrErr != nil || ctx.Err() != nil {
		return runtimebootstrap.PublicBundle{}, rejected
	}
	want, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return runtimebootstrap.PublicBundle{}, rejected
	}
	got, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(want, got) {
		return runtimebootstrap.PublicBundle{}, rejected
	}
	return runtimebootstrap.PublicBundle{CertificatePEM: response.CertificatePem, CAPEM: trust}, nil
}

func (e *experiment) install(ctx context.Context, index int, bundle runtimebootstrap.PublicBundle) error {
	return until(ctx, func() (bool, error) {
		err := e.checked(ctx, index, func() error { return e.engine.BootstrapInstallExec(ctx, e.ids[index], 1000, bundle) })
		if errors.Is(err, runtimebootstrap.ErrNotReady) {
			return false, nil
		}
		return err == nil, err
	})
}

func (e *experiment) rejectInstall(ctx context.Context, index int, bundle runtimebootstrap.PublicBundle) error {
	return until(ctx, func() (bool, error) {
		err := e.checked(ctx, index, func() error { return e.engine.BootstrapInstallExec(ctx, e.ids[index], 1000, bundle) })
		if errors.Is(err, runtimebootstrap.ErrNotReady) {
			return false, nil
		}
		if err != runtimebootstrap.ErrInstallRejected || ctx.Err() != nil {
			return false, rejected
		}
		return true, nil
	})
}

// This fixed helper only connects to 127.0.0.1:8093 and returns exit 0/1.
// Existing Engine.ExecContainer runs it as UID65532, with no identity-file
// reads. Worker and both bootstrap management commands run as UID1000.
func (e *experiment) health(ctx context.Context, index int, ready bool) error {
	check := func() (bool, error) {
		var result docker.ExecResult
		err := e.checked(ctx, index, func() error {
			var err error
			result, err = e.engine.ExecContainer(ctx, e.ids[index], []string{"/worker", "healthcheck"})
			return err
		})
		if err != nil || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
			return false, rejected
		}
		if ready {
			if result.ExitCode == 1 {
				return false, nil
			}
			if result.ExitCode != 0 {
				return false, rejected
			}
			return true, nil
		}
		if result.ExitCode != 1 {
			return false, rejected
		}
		return true, nil
	}
	if ready {
		return until(ctx, check)
	}
	_, err := check()
	return err
}

func (e *experiment) run(ctx context.Context) (summary, error) {
	e.stage = "engine-ping"
	if e.engine.Ping(ctx) != nil {
		return summary{}, rejected
	}
	e.stage = "authority-seed"
	if e.world.seed(ctx) != nil {
		return summary{}, rejected
	}
	requests := [2]runtimebootstrap.Request{}
	keys := [2][32]byte{}
	bundles := [2]runtimebootstrap.PublicBundle{}
	for index := 0; index < 2; index++ {
		var err error
		e.stage = "request"
		requests[index], keys[index], err = e.request(ctx, index)
		if err != nil {
			return summary{}, rejected
		}
		e.stage = "pre-ready"
		if e.health(ctx, index, false) != nil {
			return summary{}, rejected
		}
		e.stage = "enroll"
		bundles[index], err = e.enroll(ctx, requests[index])
		if err != nil {
			return summary{}, rejected
		}
		e.stage = "key-retry"
		retry, key, err := e.request(ctx, index)
		if err != nil || key != keys[index] || bytes.Equal(retry.CSRPEM, requests[index].CSRPEM) {
			return summary{}, rejected
		}
		e.stage = "leaf-retry"
		response, err := e.enroll(ctx, retry)
		if err != nil || !bytes.Equal(response.CertificatePEM, bundles[index].CertificatePEM) {
			return summary{}, rejected
		}
	}
	e.stage = "keys"
	if keys[0] == keys[1] {
		return summary{}, rejected
	}
	e.stage = "cross-install"
	if e.rejectInstall(ctx, 1, bundles[0]) != nil || e.health(ctx, 1, false) != nil {
		return summary{}, rejected
	}
	e.stage = "wrong-ca"
	other, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		return summary{}, rejected
	}
	wrong := runtimebootstrap.PublicBundle{CertificatePEM: bundles[0].CertificatePEM, CAPEM: other.CertificatePEM()}
	if e.rejectInstall(ctx, 0, wrong) != nil || e.health(ctx, 0, false) != nil {
		return summary{}, rejected
	}
	for index := 0; index < 2; index++ {
		e.stage = "install"
		if e.install(ctx, index, bundles[index]) != nil {
			return summary{}, rejected
		}
		e.stage = "install-retry"
		if e.install(ctx, index, bundles[index]) != nil {
			return summary{}, rejected
		}
		e.stage = "ready"
		if e.health(ctx, index, true) != nil {
			return summary{}, rejected
		}
	}
	// Revoke each authority independently: A's secondary lease, B's durable
	// lease. This tests new enrollment denial, not established TLS teardown.
	e.stage = "lease-revoke"
	if e.world.leases.Revoke(ctx, claim(0)) != nil || e.world.repository.RevokeExecutionLease(ctx, binding(1).SlotID, 1, "owner-live", time.Now()) != nil {
		return summary{}, rejected
	}
	for index := 0; index < 2; index++ {
		e.stage = "lease-denial"
		if response, err := e.enroll(ctx, requests[index]); status.Code(err) != codes.PermissionDenied || len(response.CertificatePEM) != 0 {
			return summary{}, rejected
		}
		e.stage = "final-policy"
		if inspect(ctx, e.engine, e.config, e.public, e.ids, index) != nil {
			return summary{}, rejected
		}
	}
	e.stage = "deadline"
	if ctx.Err() != nil {
		return summary{}, rejected
	}
	return summary{ActualDockerExec: true, AuthenticatedEnrollment: true, IndependentKeys: true, SameKeyLeafStable: true, CrossInstallRejected: true, WrongCARejected: true, LeaseRetryRejected: true, PublicInstallRetry: true, TCPReady: true}, nil
}
