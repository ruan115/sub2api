package runtimeenrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

const MaxNodeAge = 45 * time.Second

var sessionPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Broker struct{ config Config }

func New(config Config) (*Broker, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	if config.Bindings == nil || config.Receipts == nil || config.Authority == nil || config.Leases == nil || config.Sessions == nil ||
		config.Timeout <= 0 || config.Timeout > 5*time.Second {
		return nil, ErrRejected
	}
	return &Broker{config: config}, nil
}

// Enroll never renews a lease or publishes readiness. Persistent first-writer
// pinning happens before the last complete authority recheck and before reply.
func (b *Broker) Enroll(parent context.Context, principal Principal, request Request) ([]byte, error) {
	if b == nil || parent == nil || parent.Err() != nil || !sessionPattern.MatchString(principal.SessionID) ||
		credential.ValidateTransportID(principal.NodeID) != nil || credential.ValidateTransportID(request.SlotID) != nil ||
		request.Epoch == 0 || request.Generation == 0 || len(request.CSRPEM) == 0 || len(request.CSRPEM) > 16*1024 {
		return nil, ErrRejected
	}
	ctx, cancel := context.WithTimeout(parent, b.config.Timeout)
	defer cancel()
	started := b.config.Now().UTC()
	if started.IsZero() || ctx.Err() != nil {
		return nil, ErrRejected
	}
	first, err := b.current(ctx, principal, request, Grant{}, started)
	if err != nil {
		return nil, ErrRejected
	}
	csr, err := runtimeidentity.ValidateCSR(first.RuntimeBinding(), request.CSRPEM)
	if err != nil {
		return nil, ErrRejected
	}
	publicDER, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, ErrRejected
	}
	expected := Receipt{AssignmentID: first.AssignmentID, Binding: first.RuntimeBinding(), PublicKeySHA256: sha256.Sum256(publicDER), CASHA256: sha256.Sum256(b.config.Authority.CertificatePEM())}
	receipt, loadErr := b.config.Receipts.Load(ctx, expected.AssignmentID)
	if loadErr != nil && !errors.Is(loadErr, ErrReceiptNotFound) {
		return nil, ErrRejected
	}
	if _, err = b.current(ctx, principal, request, first, started); err != nil {
		return nil, ErrRejected
	}
	if errors.Is(loadErr, ErrReceiptNotFound) {
		issued, err := b.config.Authority.IssueRuntime(expected.Binding, request.CSRPEM)
		if err != nil {
			return nil, ErrRejected
		}
		expected.CertificatePEM = issued.CertificatePEM
		expected.NotBefore, expected.NotAfter = issued.Certificate.NotBefore, issued.Certificate.NotAfter
		if _, err = b.current(ctx, principal, request, first, started); err != nil {
			return nil, ErrRejected
		}
		receipt, err = b.config.Receipts.GetOrCreate(ctx, expected)
		if err != nil {
			return nil, ErrRejected
		}
	}
	last, err := b.current(ctx, principal, request, first, started)
	if err != nil || !SameReceiptIdentity(receipt, expected) || ValidateReceipt(receipt) != nil {
		return nil, ErrRejected
	}
	now := b.config.Now().UTC()
	leaf, err := runtimeidentity.ValidateCertificate(expected.Binding, receipt.CertificatePEM, b.config.Authority.CertificatePEM(), now)
	if err != nil {
		return nil, ErrRejected
	}
	der, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || sha256.Sum256(der) != expected.PublicKeySHA256 {
		return nil, ErrRejected
	}
	// Validation and dependencies can consume time or cancel the caller. Neither
	// refreshed node timestamps nor renewed DB leases extend this attempt.
	now = b.config.Now().UTC()
	if !attemptCurrent(ctx, now, started, b.config.Timeout, first, last) || !now.Before(leaf.NotAfter) {
		return nil, ErrRejected
	}
	return bytes.Clone(receipt.CertificatePEM), nil
}

func (b *Broker) current(ctx context.Context, p Principal, r Request, original Grant, started time.Time) (Grant, error) {
	if ctx.Err() != nil || b.config.Sessions.ValidateControlSession(ctx, p.NodeID, p.SessionID) != nil {
		return Grant{}, ErrRejected
	}
	now := b.config.Now().UTC()
	g, err := b.config.Bindings.ReadRuntimeEnrollmentBinding(ctx, r.SlotID, now, MaxNodeAge)
	if err != nil || !grantCurrent(g, p, r, now) || original.AssignmentID != "" && !sameGrant(original, g) || ctx.Err() != nil {
		return Grant{}, ErrRejected
	}
	claim := lease.Claim{SlotID: g.SlotID, NodeID: g.NodeID, ExecutionEpoch: g.Epoch, OwnerID: g.LeaseOwnerID}
	if b.config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil {
		return Grant{}, ErrRejected
	}
	after, err := b.config.Bindings.ReadRuntimeEnrollmentBinding(ctx, r.SlotID, b.config.Now().UTC(), MaxNodeAge)
	if err != nil || !sameGrant(g, after) || !grantCurrent(after, p, r, b.config.Now().UTC()) || ctx.Err() != nil {
		return Grant{}, ErrRejected
	}
	if b.config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil || b.config.Sessions.ValidateControlSession(ctx, p.NodeID, p.SessionID) != nil {
		return Grant{}, ErrRejected
	}
	now = b.config.Now().UTC()
	if original.AssignmentID == "" {
		original = g
	}
	if !grantCurrent(g, p, r, now) || !attemptCurrent(ctx, now, started, b.config.Timeout, original, g, after) {
		return Grant{}, ErrRejected
	}
	if after.NodeSeenAt.Before(g.NodeSeenAt) {
		g.NodeSeenAt = after.NodeSeenAt
	}
	if after.LeaseExpiresAt.Before(g.LeaseExpiresAt) {
		g.LeaseExpiresAt = after.LeaseExpiresAt
	}
	return g, nil
}

func grantCurrent(g Grant, p Principal, r Request, now time.Time) bool {
	for _, id := range []string{g.AssignmentID, g.AccountID, g.LeaseOwnerID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	return g.RuntimeBinding().Validate() == nil && digestPattern.MatchString(g.ImageDigest) && g.SlotID == r.SlotID && g.NodeID == p.NodeID &&
		g.ControlSessionID == p.SessionID && g.Epoch == r.Epoch && g.Generation == r.Generation &&
		!g.NodeSeenAt.IsZero() && !g.NodeSeenAt.After(now) && g.NodeSeenAt.Add(MaxNodeAge).After(now) && g.LeaseExpiresAt.After(now)
}
func sameGrant(a, b Grant) bool {
	return a.AssignmentID == b.AssignmentID && a.AccountID == b.AccountID && a.SlotID == b.SlotID && a.NodeID == b.NodeID &&
		a.ImageDigest == b.ImageDigest && a.ControlSessionID == b.ControlSessionID && a.LeaseOwnerID == b.LeaseOwnerID && a.Epoch == b.Epoch && a.Generation == b.Generation
}
func attemptCurrent(ctx context.Context, now, started time.Time, timeout time.Duration, grants ...Grant) bool {
	if now.Before(started) || !started.Add(timeout).After(now) {
		return false
	}
	for _, g := range grants {
		if !g.LeaseExpiresAt.After(now) || g.NodeSeenAt.After(now) || !g.NodeSeenAt.Add(MaxNodeAge).After(now) {
			return false
		}
	}
	return ctx.Err() == nil
}
