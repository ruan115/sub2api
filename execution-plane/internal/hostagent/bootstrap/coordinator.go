// Package bootstrap connects the existing provider and authenticated control
// channel without introducing a second listener or private-key transport.
package bootstrap

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrBootstrap = errors.New("authenticated runtime bootstrap failed")

type Transport interface {
	BootstrapRequest(context.Context, provider.Instance, provider.SlotSpec) (runtimebootstrap.Request, error)
	BootstrapInstall(context.Context, provider.Instance, provider.SlotSpec, runtimebootstrap.PublicBundle) error
}

type EnrollmentClient interface {
	Enroll(context.Context, runtimebootstrap.Request) (runtimebootstrap.PublicBundle, error)
}

type Config struct {
	Transport Transport
	Client    EnrollmentClient
	NodeID    string
	TrustPEM  []byte
}

type Coordinator struct{ config Config }

func New(config Config) (*Coordinator, error) {
	if config.Transport == nil || config.Client == nil || len(config.TrustPEM) == 0 || len(config.TrustPEM) > 16<<10 ||
		(runtimeidentity.Binding{AccountHash: "00000000000000000000000000000000", SlotID: "validation", NodeID: config.NodeID, Epoch: 1, Generation: 1}).Validate() != nil {
		return nil, ErrBootstrap
	}
	config.TrustPEM = bytes.Clone(config.TrustPEM)
	return &Coordinator{config: config}, nil
}

func (c *Coordinator) Prepare(parent context.Context, spec provider.SlotSpec, instance provider.Instance) error {
	if c == nil || parent == nil || parent.Err() != nil || spec.Validate() != nil || instance.ProviderRef == "" ||
		instance.SlotID != spec.SlotID || instance.Epoch != spec.Epoch || instance.RuntimeGeneration != spec.RuntimeGeneration {
		return ErrBootstrap
	}
	ctx, cancel := context.WithTimeout(parent, runtimebootstrap.DefaultTimeout)
	defer cancel()
	expected := runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID, NodeID: c.config.NodeID, Epoch: spec.Epoch, Generation: spec.RuntimeGeneration}
	request, err := c.request(ctx, spec, instance)
	if err != nil || request.Binding != expected || ctx.Err() != nil {
		return ErrBootstrap
	}
	csr, err := runtimeidentity.ValidateCSR(expected, request.CSRPEM)
	if err != nil {
		return ErrBootstrap
	}
	bundle, err := c.config.Client.Enroll(ctx, request)
	if err != nil || ctx.Err() != nil || !bytes.Equal(bundle.CAPEM, c.config.TrustPEM) {
		return ErrBootstrap
	}
	leaf, err := runtimeidentity.ValidateCertificate(expected, bundle.CertificatePEM, c.config.TrustPEM, time.Now())
	if err != nil {
		return ErrBootstrap
	}
	want, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return ErrBootstrap
	}
	got, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(want, got) || ctx.Err() != nil {
		return ErrBootstrap
	}
	if c.install(ctx, instance, spec, bundle) != nil || ctx.Err() != nil {
		return ErrBootstrap
	}
	return nil
}

// Wait and the fixed install command share a nonblocking instance lock.
// Retry only this transient condition with the exact same public bundle.
func (c *Coordinator) install(ctx context.Context, instance provider.Instance, spec provider.SlotSpec, bundle runtimebootstrap.PublicBundle) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := c.config.Transport.BootstrapInstall(ctx, instance, spec, bundle)
		if !errors.Is(err, runtimebootstrap.ErrNotReady) {
			return err
		}
		select {
		case <-ctx.Done():
			return ErrBootstrap
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) request(ctx context.Context, spec provider.SlotSpec, instance provider.Instance) (runtimebootstrap.Request, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := c.config.Transport.BootstrapRequest(ctx, instance, spec)
		if !errors.Is(err, runtimebootstrap.ErrNotReady) {
			return request, err
		}
		select {
		case <-ctx.Done():
			return runtimebootstrap.Request{}, ErrBootstrap
		case <-ticker.C:
		}
	}
}
