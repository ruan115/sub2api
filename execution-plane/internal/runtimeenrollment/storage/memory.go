// Package storage persists first-issued public runtime certificates. It does
// not determine assignment authority, renew certificates or store private keys.
package storage

import (
	"context"
	"sync"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	enrollment "github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
)

// Memory is bounded and test-only. A production process must use durable SQL
// receipts; otherwise restart would discard the first-key pin.
type Memory struct {
	mu       sync.Mutex
	capacity int
	receipts map[string]enrollment.Receipt
}

func NewMemory(maxReceipts int) (*Memory, error) {
	if maxReceipts <= 0 || maxReceipts > 10000 {
		return nil, enrollment.ErrRejected
	}
	return &Memory{capacity: maxReceipts, receipts: make(map[string]enrollment.Receipt)}, nil
}
func (m *Memory) Load(ctx context.Context, id string) (enrollment.Receipt, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(id) != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	r, ok := m.receipts[id]
	if !ok {
		return enrollment.Receipt{}, enrollment.ErrReceiptNotFound
	}
	return enrollment.CloneReceipt(r), nil
}
func (m *Memory) GetOrCreate(ctx context.Context, candidate enrollment.Receipt) (enrollment.Receipt, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || enrollment.ValidateReceipt(candidate) != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	if r, ok := m.receipts[candidate.AssignmentID]; ok {
		if !enrollment.SameReceiptIdentity(r, candidate) {
			return enrollment.Receipt{}, enrollment.ErrRejected
		}
		return enrollment.CloneReceipt(r), nil
	}
	if len(m.receipts) >= m.capacity {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	for _, r := range m.receipts {
		if r.Binding.SlotID == candidate.Binding.SlotID && r.Binding.Epoch == candidate.Binding.Epoch {
			return enrollment.Receipt{}, enrollment.ErrRejected
		}
	}
	m.receipts[candidate.AssignmentID] = enrollment.CloneReceipt(candidate)
	return enrollment.CloneReceipt(candidate), nil
}

var _ enrollment.ReceiptStore = (*Memory)(nil)
