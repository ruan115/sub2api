// Package lifecycle composes authenticated START commands for existing
// instances. It does not enable a daemon, business execution or a ticket issuer.
package lifecycle

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent/bootstrap"
)

var ErrComposition = errors.New("authenticated slot lifecycle configuration rejected")

type Config struct {
	Commands        hostagent.SlotCommandExecutorConfig
	NodeID          string
	TrustPEM        []byte
	NodeCertificate tls.Certificate
	Enrollment      bootstrap.EnrollmentClient
	ReadyTimeout    time.Duration
	// Custody is optional. Without it START behaves exactly as before: it owns
	// only a short-lived authenticated connection and closes it. With it, the
	// connection is held for as long as the execution lease authority confirms
	// the claim, and reclaimed when it stops confirming it.
	Custody *Custody
}

type runtimeProvider interface {
	hostagent.SlotCommandProvider
	hostagent.ExistingRuntimeProvider
	bootstrap.Transport
}

// New takes a single provider for command observations, immutable-instance
// validation, bootstrap transport and TLS endpoint resolution. No caller can
// inject a different Startup, skip bootstrap, or supply a host signing key.
func New(config Config) (*hostagent.SlotCommandExecutor, error) {
	p, ok := config.Commands.Provider.(runtimeProvider)
	if !ok || isNil(p) || isNil(config.Enrollment) || config.Commands.Startup != nil ||
		config.ReadyTimeout <= 0 || config.ReadyTimeout > time.Minute {
		return nil, ErrComposition
	}
	coordinator, err := bootstrap.New(bootstrap.Config{Transport: p, Client: config.Enrollment,
		NodeID: config.NodeID, TrustPEM: config.TrustPEM})
	if err != nil {
		return nil, ErrComposition
	}
	controller, err := hostagent.NewController(hostagent.ControllerConfig{
		Provider: p, Bootstrap: coordinator, TicketSource: denyTickets{}, NodeID: config.NodeID,
		RuntimeTrustPEM: config.TrustPEM, NodeCertificate: config.NodeCertificate,
		ReadyTimeout: config.ReadyTimeout,
	})
	if err != nil {
		return nil, ErrComposition
	}
	commands := config.Commands
	commands.Startup = &startup{controller: controller, custody: config.Custody}
	executor, err := hostagent.NewSlotCommandExecutor(commands)
	if err != nil {
		return nil, ErrComposition
	}
	return executor, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type denyTickets struct{}

func (denyTickets) Issue(context.Context, hostagent.TicketRequest) (string, error) {
	return "", errors.New("business tickets are unavailable in slot startup")
}
