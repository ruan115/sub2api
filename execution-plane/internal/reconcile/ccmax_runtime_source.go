package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var ErrCCMAXRuntimeDesired = errors.New("CCMAX runtime desired state is invalid")

// CCMAXRuntimeDefaults contains deployment-controlled immutable slot inputs.
// CCMAX owns the account/generation/slot/provider identity; image and resource
// policy remain orchestrator configuration rather than mutable outbox payload.
type CCMAXRuntimeDefaults struct {
	RequiredLabels     map[string]string
	ImageDigest        string
	CPURequestMillis   uint64
	MemoryRequestBytes uint64
}

type MySQLAccountRuntimeSource struct {
	db       *sql.DB
	defaults CCMAXRuntimeDefaults
}

func NewMySQLAccountRuntimeSource(db *sql.DB, defaults CCMAXRuntimeDefaults) (*MySQLAccountRuntimeSource, error) {
	if db == nil || validateCCMAXRuntimeDefaults(defaults) != nil {
		return nil, ErrCCMAXRuntimeDesired
	}
	defaults.RequiredLabels = cloneRuntimeLabels(defaults.RequiredLabels)
	return &MySQLAccountRuntimeSource{db: db, defaults: defaults}, nil
}

// LoadAccountRuntimeDesired accepts a historical event generation when CCMAX
// has already advanced farther. The global outbox checkpoint still replays
// every generation in sequence, while worker_runtime's generation fence keeps
// stale projections from overwriting newer state.
func (s *MySQLAccountRuntimeSource) LoadAccountRuntimeDesired(
	ctx context.Context,
	accountID int64,
	desiredGeneration uint64,
) (AccountRuntimeDesired, error) {
	if s == nil || s.db == nil || ctx == nil || ctx.Err() != nil || accountID <= 0 || desiredGeneration == 0 {
		return AccountRuntimeDesired{}, ErrCCMAXRuntimeDesired
	}
	var currentGeneration uint64
	var slotID, provider string
	err := s.db.QueryRowContext(ctx, `
SELECT runtime_generation, runtime_slot_id, runtime_provider
FROM accounts
WHERE id = ?`, accountID).Scan(&currentGeneration, &slotID, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountRuntimeDesired{}, ErrCCMAXRuntimeDesired
	}
	if err != nil {
		return AccountRuntimeDesired{}, fmt.Errorf("load CCMAX account runtime identity: %w", err)
	}
	expectedSlotID := fmt.Sprintf("ccmax-account-%d", accountID)
	if currentGeneration < desiredGeneration || slotID != expectedSlotID || provider != "docker" {
		return AccountRuntimeDesired{}, ErrCCMAXRuntimeDesired
	}
	return AccountRuntimeDesired{
		AccountID: accountID, SlotID: slotID, Provider: provider, DesiredGeneration: desiredGeneration,
		RequiredLabels: cloneRuntimeLabels(s.defaults.RequiredLabels), ImageDigest: s.defaults.ImageDigest,
		CPURequestMillis: s.defaults.CPURequestMillis, MemoryRequestBytes: s.defaults.MemoryRequestBytes,
	}, nil
}

func validateCCMAXRuntimeDefaults(defaults CCMAXRuntimeDefaults) error {
	if len(defaults.RequiredLabels) > 32 {
		return ErrCCMAXRuntimeDesired
	}
	for key, value := range defaults.RequiredLabels {
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) || len(key) > 64 || len(value) > 128 {
			return ErrCCMAXRuntimeDesired
		}
	}
	// Reuse the durable store validator so source and projection cannot disagree
	// on image/provider/resource constraints.
	now := time.Unix(1, 0).UTC()
	repository := store.NewMemoryRepository()
	_, err := repository.PutDesiredSlot(context.Background(), store.Slot{
		ID: "ccmax-account-1", AccountID: "1", Provider: "docker", DesiredState: "ready", DesiredGeneration: 1,
		RequiredLabels: defaults.RequiredLabels, ImageDigest: defaults.ImageDigest,
		CPURequestMillis: defaults.CPURequestMillis, MemoryRequestBytes: defaults.MemoryRequestBytes,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return ErrCCMAXRuntimeDesired
	}
	return nil
}

func cloneRuntimeLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

var _ AccountRuntimeSource = (*MySQLAccountRuntimeSource)(nil)
