package bootstrap

import (
	"bytes"
	"context"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

// RPCClient uses the existing authenticated node-control connection. Its CA is
// pinned by host configuration, never supplied or replaced by the response.
type RPCClient struct {
	control executionv1.NodeControlServiceClient
	trust   []byte
}

func NewRPCClient(control executionv1.NodeControlServiceClient, trust []byte) (*RPCClient, error) {
	if control == nil || len(trust) == 0 || len(trust) > 16<<10 {
		return nil, ErrBootstrap
	}
	return &RPCClient{control: control, trust: bytes.Clone(trust)}, nil
}

func (c *RPCClient) Enroll(ctx context.Context, request runtimebootstrap.Request) (runtimebootstrap.PublicBundle, error) {
	if c == nil || ctx == nil || ctx.Err() != nil {
		return runtimebootstrap.PublicBundle{}, ErrBootstrap
	}
	if _, err := runtimeidentity.ValidateCSR(request.Binding, request.CSRPEM); err != nil {
		return runtimebootstrap.PublicBundle{}, ErrBootstrap
	}
	response, err := c.control.EnrollRuntimeCertificate(ctx, &executionv1.EnrollRuntimeCertificateRequest{
		SlotId: request.Binding.SlotID, ExecutionEpoch: request.Binding.Epoch, RuntimeGeneration: request.Binding.Generation, CsrPem: request.CSRPEM,
	})
	if err != nil || ctx.Err() != nil || len(response.GetCertificatePem()) == 0 || len(response.GetCertificatePem()) > 16<<10 {
		return runtimebootstrap.PublicBundle{}, ErrBootstrap
	}
	return runtimebootstrap.PublicBundle{CertificatePEM: bytes.Clone(response.GetCertificatePem()), CAPEM: bytes.Clone(c.trust)}, nil
}
