package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

type Provider struct {
	mu        sync.RWMutex
	instances map[string]base.Instance
	bySlot    map[string]string
	now       func() time.Time
}

func New() *Provider {
	return &Provider{
		instances: make(map[string]base.Instance),
		bySlot:    make(map[string]string),
		now:       time.Now,
	}
}

func (p *Provider) Create(_ context.Context, spec base.SlotSpec) (base.Instance, error) {
	if err := spec.Validate(); err != nil {
		return base.Instance{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if ref, ok := p.bySlot[spec.SlotID]; ok {
		instance := p.instances[ref]
		if instance.Epoch != spec.Epoch {
			return base.Instance{}, fmt.Errorf("slot %q already exists at epoch %d", spec.SlotID, instance.Epoch)
		}
		return instance, nil
	}

	now := p.now().UTC()
	ref := "fake://" + spec.SlotID
	instance := base.Instance{
		ProviderRef: ref,
		SlotID:      spec.SlotID,
		Epoch:       spec.Epoch,
		State:       slot.StateStopped,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	p.instances[ref] = instance
	p.bySlot[spec.SlotID] = ref
	return instance, nil
}

func (p *Provider) Inspect(_ context.Context, providerRef string) (base.Status, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	instance, ok := p.instances[providerRef]
	if !ok {
		return base.Status{}, base.ErrNotFound
	}
	return base.Status{
		Instance: instance,
		Healthy:  instance.State == slot.StateReady || instance.State == slot.StateBusy,
	}, nil
}

func (p *Provider) InspectSlot(ctx context.Context, slotID string) (base.Status, error) {
	if slotID == "" {
		return base.Status{}, base.ErrNotFound
	}
	p.mu.RLock()
	providerRef, ok := p.bySlot[slotID]
	p.mu.RUnlock()
	if !ok {
		return base.Status{}, base.ErrNotFound
	}
	return p.Inspect(ctx, providerRef)
}

func (p *Provider) Start(_ context.Context, providerRef string) error {
	return p.setState(providerRef, slot.StateReady)
}

func (p *Provider) Drain(_ context.Context, providerRef string, deadline time.Time) error {
	if deadline.IsZero() {
		return fmt.Errorf("drain deadline is required")
	}
	return p.setState(providerRef, slot.StateDraining)
}

func (p *Provider) Stop(_ context.Context, providerRef string) error {
	return p.setState(providerRef, slot.StateStopped)
}

func (p *Provider) Destroy(_ context.Context, providerRef string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	instance, ok := p.instances[providerRef]
	if !ok {
		return nil
	}
	delete(p.bySlot, instance.SlotID)
	delete(p.instances, providerRef)
	return nil
}

func (p *Provider) setState(providerRef string, state slot.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	instance, ok := p.instances[providerRef]
	if !ok {
		return base.ErrNotFound
	}
	instance.State = state
	instance.UpdatedAt = p.now().UTC()
	p.instances[providerRef] = instance
	return nil
}

var _ base.ExecutionProvider = (*Provider)(nil)
var _ base.SlotInspector = (*Provider)(nil)
