package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// RuntimeEnrollmentConfig is deliberately opt-in. All authoritative gates
// must be supplied; a CA alone must never turn into an unauthenticated issuer.
type RuntimeEnrollmentConfig struct {
	Bindings runtimeenrollment.BindingSource
	Receipts runtimeenrollment.ReceiptStore
	Leases   runtimeenrollment.LeaseValidator
	Timeout  time.Duration
}

func (s *Server) EnrollRuntimeCertificate(ctx context.Context, request *executionv1.EnrollRuntimeCertificateRequest) (*executionv1.EnrollRuntimeCertificateResponse, error) {
	if s.runtimeEnrollment == nil {
		return nil, status.Error(codes.Unimplemented, "runtime enrollment disabled")
	}
	identity, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	remote, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "runtime enrollment rejected")
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.Version != tls.VersionTLS13 || len(info.State.PeerCertificates) != 1 ||
		runtimeidentity.ValidateNodeCertificate(identity.nodeID, identity.certificate, s.authority.CertificatePEM(), s.config.Now()) != nil {
		return nil, status.Error(codes.Unauthenticated, "runtime enrollment rejected")
	}
	if request == nil || len(request.GetSlotId()) > 64 || request.GetExecutionEpoch() == 0 ||
		request.GetRuntimeGeneration() == 0 || len(request.GetCsrPem()) == 0 || len(request.GetCsrPem()) > 16<<10 {
		return nil, status.Error(codes.InvalidArgument, "invalid runtime enrollment request")
	}
	s.mu.RLock()
	session := s.sessions[identity.nodeID]
	valid := liveSession(session, sessionID(session)) && sameSessionCertificate(session, identity.certificate.Raw)
	s.mu.RUnlock()
	if !valid {
		return nil, status.Error(codes.PermissionDenied, "runtime enrollment rejected")
	}
	bound, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(session.context, cancel)
	defer stop()
	leaf, err := s.runtimeEnrollment.Enroll(bound, runtimeenrollment.Principal{NodeID: identity.nodeID, SessionID: session.id}, runtimeenrollment.Request{
		SlotID: request.GetSlotId(), Epoch: request.GetExecutionEpoch(), Generation: request.GetRuntimeGeneration(), CSRPEM: request.GetCsrPem(),
	})
	if err != nil || s.ValidateControlSession(bound, identity.nodeID, session.id) != nil || bound.Err() != nil {
		return nil, status.Error(codes.PermissionDenied, "runtime enrollment rejected")
	}
	s.mu.RLock()
	valid = s.sessions[identity.nodeID] == session && liveSession(session, session.id) && sameSessionCertificate(session, identity.certificate.Raw)
	s.mu.RUnlock()
	if !valid || bound.Err() != nil {
		return nil, status.Error(codes.PermissionDenied, "runtime enrollment rejected")
	}
	return &executionv1.EnrollRuntimeCertificateResponse{CertificatePem: bytes.Clone(leaf)}, nil
}

func sessionID(session *nodeSession) string {
	if session == nil {
		return ""
	}
	return session.id
}

func sameSessionCertificate(session *nodeSession, raw []byte) bool {
	if session == nil {
		return false
	}
	remote, ok := peer.FromContext(session.context)
	if !ok {
		return false
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	return ok && info.State.Version == tls.VersionTLS13 && len(info.State.PeerCertificates) == 1 &&
		bytes.Equal(info.State.PeerCertificates[0].Raw, raw)
}
