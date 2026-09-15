package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var ErrControlSessionUnavailable = errors.New("execution control session is unavailable")

// ValidateControlSession is a read-only control-plane gate, not a replacement
// for the assignment or execution lease authority. A durable connected row is
// insufficient: the same authenticated stream must still be live locally.
// Remote host-agents must access this authority through an authenticated RPC;
// they must not receive the repository or the control server's signing keys.
func (s *Server) ValidateControlSession(ctx context.Context, nodeID, sessionID string) error {
	if s == nil || s.repository == nil || s.config.Now == nil || ctx == nil || ctx.Err() != nil ||
		!nodeIDPattern.MatchString(nodeID) || len(sessionID) != 32 {
		return ErrControlSessionUnavailable
	}
	s.mu.RLock()
	session := s.sessions[nodeID]
	current := liveSession(session, sessionID)
	s.mu.RUnlock()
	if !current {
		return ErrControlSessionUnavailable
	}
	remote, ok := peer.FromContext(session.context)
	if !ok || remote.AuthInfo == nil {
		return ErrControlSessionUnavailable
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.Version != tls.VersionTLS13 || len(info.State.PeerCertificates) == 0 ||
		len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return ErrControlSessionUnavailable
	}
	leaf := info.State.PeerCertificates[0]
	verified := info.State.VerifiedChains[0][0]
	if leaf == nil || verified == nil || !bytes.Equal(leaf.Raw, verified.Raw) {
		return ErrControlSessionUnavailable
	}
	identity, err := pki.NodeIDFromCertificate(leaf)
	now := s.config.Now().UTC()
	if err != nil || identity != nodeID || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) ||
		s.repository.ValidateCertificate(ctx, nodeID, pki.SerialString(leaf.SerialNumber), now) != nil {
		return ErrControlSessionUnavailable
	}
	node, err := s.repository.GetNode(ctx, nodeID)
	if err != nil || node.ID != nodeID || node.Status != "connected" || node.ControlSessionID != sessionID {
		return ErrControlSessionUnavailable
	}
	// Storage validation can block. Do not accept the old stream after a
	// disconnect, replacement, cancellation or certificate expiry during I/O.
	s.mu.RLock()
	current = s.sessions[nodeID] == session && liveSession(session, sessionID)
	s.mu.RUnlock()
	now = s.config.Now().UTC()
	if !current || ctx.Err() != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return ErrControlSessionUnavailable
	}
	return nil
}

// Called while holding Server.mu; session.context and done are immutable.
func liveSession(session *nodeSession, sessionID string) bool {
	if session == nil || !session.accepted || session.id != sessionID || session.context == nil || session.context.Err() != nil || session.done == nil {
		return false
	}
	select {
	case <-session.done:
		return false
	default:
		return true
	}
}
