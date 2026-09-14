package dataplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func authorize(ctx context.Context) error {
	denied := status.Error(codes.PermissionDenied, "CCMAX service peer is unauthorized")
	if ctx == nil {
		return denied
	}
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return denied
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.Version != tls.VersionTLS13 || len(info.State.PeerCertificates) == 0 ||
		len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return denied
	}
	leaf := info.State.PeerCertificates[0]
	if leaf == nil || info.State.VerifiedChains[0][0] == nil || !bytes.Equal(leaf.Raw, info.State.VerifiedChains[0][0].Raw) {
		return denied
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return denied
	}
	id, err := pki.ServiceIDFromCertificate(leaf)
	if err != nil || id != "ccmax" {
		return denied
	}
	return nil
}

// NewGRPCServer is an optional, non-listening assembly helper. No caller option
// can weaken the TLS or message-size limits. Method authorization also remains
// mandatory when Register is used with another registrar.
func NewGRPCServer(config Config, transport *tls.Config) (*grpc.Server, error) {
	server, err := NewServer(config)
	if err != nil {
		return nil, err
	}
	if transport == nil || transport.MinVersion != tls.VersionTLS13 ||
		(transport.MaxVersion != 0 && transport.MaxVersion != tls.VersionTLS13) ||
		transport.ClientAuth != tls.RequireAndVerifyClientCert || transport.ClientCAs == nil ||
		len(transport.ClientCAs.Subjects()) == 0 || len(transport.Certificates) == 0 ||
		transport.GetConfigForClient != nil {
		return nil, errors.New("data-plane TLS configuration is invalid")
	}
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(transport.Clone())),
		grpc.MaxRecvMsgSize(server.config.MaxMessageBytes), grpc.MaxSendMsgSize(server.config.MaxMessageBytes))
	server.Register(rpc)
	return rpc, nil
}
