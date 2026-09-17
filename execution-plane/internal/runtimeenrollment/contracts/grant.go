// Package contracts contains the acyclic projection shared by the runtime
// repository and enrollment broker. It is not an enrollment authority.
package contracts

import (
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

type Grant struct {
	AssignmentID, AccountID, SlotID, NodeID, ImageDigest, ControlSessionID, LeaseOwnerID string
	Epoch, Generation                                                                    uint64
	NodeSeenAt, LeaseExpiresAt                                                           time.Time
}

func (g Grant) RuntimeBinding() runtimeidentity.Binding {
	return runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID(g.AccountID), SlotID: g.SlotID,
		NodeID: g.NodeID, Epoch: g.Epoch, Generation: g.Generation}
}
