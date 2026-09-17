// Package runtimeenrollment authorizes only a runtime TLS leaf certificate.
// Callers must obtain Principal from authenticated control-plane state, never
// directly from caller-provided node or session fields.
package runtimeenrollment

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/contracts"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrRejected = errors.New("runtime enrollment rejected")
var ErrReceiptNotFound = errors.New("runtime enrollment receipt not found")

type Grant = contracts.Grant
type Request struct {
	SlotID            string
	Epoch, Generation uint64
	CSRPEM            []byte
}
type Principal struct{ NodeID, SessionID string }

type BindingSource interface {
	ReadRuntimeEnrollmentBinding(context.Context, string, time.Time, time.Duration) (Grant, error)
}
type SessionValidator interface {
	ValidateControlSession(context.Context, string, string) error
}
type LeaseValidator interface {
	Validate(context.Context, lease.Claim) error
}

// Receipt pins the first public key and exact leaf for this assignment. It
// contains no private key, CSR secret, execution ticket or credential material.
type Receipt struct {
	AssignmentID        string
	Binding             runtimeidentity.Binding
	PublicKeySHA256     [32]byte
	CASHA256            [32]byte
	NotBefore, NotAfter time.Time
	CertificatePEM      []byte
}
type ReceiptStore interface {
	Load(context.Context, string) (Receipt, error)
	GetOrCreate(context.Context, Receipt) (Receipt, error)
}
type Config struct {
	Bindings  BindingSource
	Receipts  ReceiptStore
	Authority *pki.Authority
	Leases    LeaseValidator
	Sessions  SessionValidator
	Now       func() time.Time
	Timeout   time.Duration
}
