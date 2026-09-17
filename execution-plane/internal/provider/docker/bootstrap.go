package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	bootstrapexec "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker/bootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

type bootstrapEngine interface {
	BootstrapRequestExec(context.Context, string, uint32) ([]byte, error)
	BootstrapInstallExec(context.Context, string, uint32, runtimebootstrap.PublicBundle) error
}

func (e *HTTPEngine) BootstrapRequestExec(ctx context.Context, cid string, uid uint32) ([]byte, error) {
	return bootstrapexec.Request(ctx, cid, uid, e.do)
}

func (e *HTTPEngine) BootstrapInstallExec(ctx context.Context, cid string, uid uint32, bundle runtimebootstrap.PublicBundle) error {
	return bootstrapexec.Install(ctx, cid, uid, bundle, e.do)
}

func (p *Provider) bootstrapContainer(ctx context.Context, instance base.Instance, spec base.SlotSpec) (Container, runtimeidentity.Binding, error) {
	if ctx == nil || ctx.Err() != nil || spec.Validate() != nil || p.config.WorkerBootstrap == nil || !runtimebootstrap.ValidTrustPin(p.config.WorkerBootstrap.TrustSHA256) ||
		instance.ProviderRef == "" || instance.SlotID != spec.SlotID || instance.Epoch != spec.Epoch || instance.RuntimeGeneration != spec.RuntimeGeneration {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrBootstrap
	}
	binding := runtimeidentity.Binding{AccountHash: base.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID,
		NodeID: p.config.WorkerBootstrap.NodeID, Epoch: spec.Epoch, Generation: spec.RuntimeGeneration}
	if binding.Validate() != nil {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrBootstrap
	}
	// existingSlot compares resource/egress/user/image policy as well as identity.
	if _, err := p.existingSlot(ctx, instance.ProviderRef, spec); err != nil {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrBootstrap
	}
	container, err := p.readSandbox(ctx, instance.ProviderRef)
	if err != nil {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrBootstrap
	}
	status, err := p.statusFromContainer(container, instance.ProviderRef)
	uid := strconv.FormatUint(uint64(spec.Security.RunAsUser), 10)
	if err != nil || status.SlotID != spec.SlotID || status.Epoch != spec.Epoch || status.RuntimeGeneration != spec.RuntimeGeneration || status.ImageDigest != spec.ImageDigest ||
		container.Config.Labels[labelAccountHash] != binding.AccountHash || container.Config.User != uid+":"+uid {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrBootstrap
	}
	if !container.State.Running {
		return Container{}, runtimeidentity.Binding{}, runtimebootstrap.ErrNotReady
	}
	return container, binding, nil
}

func (p *Provider) BootstrapRequest(ctx context.Context, instance base.Instance, spec base.SlotSpec) (runtimebootstrap.Request, error) {
	engine, ok := p.engine.(bootstrapEngine)
	if !ok {
		return runtimebootstrap.Request{}, runtimebootstrap.ErrBootstrap
	}
	container, binding, err := p.bootstrapContainer(ctx, instance, spec)
	if err != nil {
		return runtimebootstrap.Request{}, err
	}
	data, execErr := engine.BootstrapRequestExec(ctx, container.ID, spec.Security.RunAsUser)
	after, _, checkErr := p.bootstrapContainer(ctx, instance, spec)
	if checkErr != nil || after.ID != container.ID {
		return runtimebootstrap.Request{}, runtimebootstrap.ErrBootstrap
	}
	if execErr != nil {
		return runtimebootstrap.Request{}, execErr
	}
	request, err := runtimebootstrap.DecodeRequest(data)
	if err != nil || request.Binding != binding {
		return runtimebootstrap.Request{}, runtimebootstrap.ErrBootstrap
	}
	return request, nil
}

func (p *Provider) BootstrapInstall(ctx context.Context, instance base.Instance, spec base.SlotSpec, bundle runtimebootstrap.PublicBundle) error {
	engine, ok := p.engine.(bootstrapEngine)
	if !ok {
		return runtimebootstrap.ErrBootstrap
	}
	container, binding, err := p.bootstrapContainer(ctx, instance, spec)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(bundle.CAPEM)
	if hex.EncodeToString(digest[:]) != p.config.WorkerBootstrap.TrustSHA256 {
		return runtimebootstrap.ErrBootstrap
	}
	if _, err := runtimeidentity.ValidateCertificate(binding, bundle.CertificatePEM, bundle.CAPEM, time.Now()); err != nil {
		return runtimebootstrap.ErrBootstrap
	}
	execErr := engine.BootstrapInstallExec(ctx, container.ID, spec.Security.RunAsUser, bundle)
	after, _, checkErr := p.bootstrapContainer(ctx, instance, spec)
	if checkErr != nil || after.ID != container.ID {
		return runtimebootstrap.ErrBootstrap
	}
	return execErr
}
