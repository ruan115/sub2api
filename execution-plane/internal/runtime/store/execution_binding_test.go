package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const bindingSessionOne = "11111111111111111111111111111111"
const bindingSessionTwo = "22222222222222222222222222222222"

func executionBindingFixture(t *testing.T) (*MemoryRepository, time.Time) {
	t.Helper()
	r, now := connectedMemoryRepository(t)
	node, err := r.GetNode(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AcceptHello(context.Background(), Hello{NodeID: node.ID, SessionID: bindingSessionOne,
		ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PutDesiredSlot(context.Background(), desiredSlot("slot-1", "account-1", 1, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: node.ID, ExpectedNodeSessionID: bindingSessionOne,
		NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GrantExecutionLease(context.Background(), ExecutionLease{
		ID: "lease-1", SlotID: "slot-1", NodeID: node.ID, ExecutionEpoch: 1, OwnerID: "owner-1",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyCommandResult(context.Background(), bindingResult("command-1", bindingSessionOne, now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	return r, now.Add(2 * time.Second)
}

func bindingResult(commandID, session string, at time.Time) CommandResult {
	return CommandResult{
		CommandID: commandID, NodeID: "srv74", ControlSessionID: session, Succeeded: true,
		ExpectedImageDigest: "sha256:" + strings.Repeat("a", 64),
		Observation: &AssignmentObservation{SlotID: "slot-1", ExecutionEpoch: 1,
			ProviderRef: "fake://slot-1", ActualState: "running", Healthy: true, ObservedAt: at},
		ReceivedAt: at, RetryAt: at.Add(5 * time.Second),
	}
}

func requireBindingDenied(t *testing.T, binding ExecutionBinding, err error) {
	t.Helper()
	if binding != (ExecutionBinding{}) || err != ErrExecutionBindingUnavailable {
		t.Fatalf("expected empty denied binding, got %+v / %v", binding, err)
	}
}

func TestReadExecutionBindingMemoryPreservesObservationAndCopiesValues(t *testing.T) {
	r, now := executionBindingFixture(t)
	got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := ExecutionBinding{AccountID: "account-1", SlotID: "slot-1", NodeID: "srv74", ProviderRef: "fake://slot-1",
		LeaseOwnerID: "owner-1", ControlSessionID: bindingSessionOne, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		ExecutionEpoch: 1, RouteGeneration: 1, ObservedAt: now.Add(-time.Second), NodeSeenAt: now.Add(-2 * time.Second), LeaseExpiresAt: now.Add(58 * time.Second)}
	if got != want {
		t.Fatalf("binding = %+v, want %+v", got, want)
	}
	got.ObservedAt = now
	got.AccountID = "changed-local-copy"
	next, err := r.ReadExecutionBinding(context.Background(), "slot-1", now.Add(time.Second), 45*time.Second)
	if err != nil || next != want {
		t.Fatal("read refreshed observation or returned mutable repository state")
	}
}

func TestReadExecutionBindingMemoryReconnectNeedsNewSessionObservation(t *testing.T) {
	r, now := executionBindingFixture(t)
	if err := r.MarkDisconnected(context.Background(), "srv74", bindingSessionOne, now); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	requireBindingDenied(t, got, err)
	node, _ := r.GetNode(context.Background(), "srv74")
	if err := r.AcceptHello(context.Background(), Hello{NodeID: node.ID, SessionID: bindingSessionTwo,
		ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	requireBindingDenied(t, got, err)
	if err := r.RecordHeartbeat(context.Background(), Heartbeat{NodeID: node.ID, SessionID: bindingSessionTwo, ReceivedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReadExecutionBinding(context.Background(), "slot-1", now.Add(time.Second), 45*time.Second)
	requireBindingDenied(t, got, err)
	if err := r.ApplyCommandResult(context.Background(), bindingResult("late-command", bindingSessionOne, now.Add(time.Second))); err == nil {
		t.Fatal("old control session restored its observation")
	}
	got, err = r.ReadExecutionBinding(context.Background(), "slot-1", now.Add(time.Second), 45*time.Second)
	requireBindingDenied(t, got, err)
	if err := r.ApplyCommandResult(context.Background(), bindingResult("current-command", bindingSessionTwo, now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReadExecutionBinding(context.Background(), "slot-1", now.Add(time.Second), 45*time.Second)
	if err != nil || got.ControlSessionID != bindingSessionTwo || !got.ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("current authenticated observation did not restore proof: %+v / %v", got, err)
	}
	if _, err := r.ObserveAssignment(context.Background(), AssignmentObservation{SlotID: "slot-1", ExecutionEpoch: 1,
		ProviderRef: "fake://slot-1", ActualState: "running", Healthy: true, ObservedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReadExecutionBinding(context.Background(), "slot-1", now.Add(2*time.Second), 45*time.Second)
	requireBindingDenied(t, got, err)
}

func TestReadExecutionBindingMemoryRejectsInconsistentAndExpiredState(t *testing.T) {
	changes := map[string]func(*Slot, *Assignment, *Node, *ExecutionLease, time.Time){
		"slot identity":   func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.ID = "another-slot" },
		"empty account":   func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.AccountID = "" },
		"account control": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.AccountID = "bad\naccount" },
		"account length": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.AccountID = strings.Repeat("a", 129)
		},
		"desired stopped": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredState = "stopped" },
		"generation zero": func(s *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.DesiredGeneration, a.DesiredGeneration = 0, 0
		},
		"generation changed": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredGeneration++ },
		"assignment slot":    func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.SlotID = "another-slot" },
		"assignment node":    func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.NodeID = "another-node" },
		"epoch mismatch":     func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ExecutionEpoch++ },
		"released":           func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) { a.ReleasedAt = &now },
		"unhealthy":          func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.Healthy = false },
		"draining":           func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ActualState = "draining" },
		"empty ref":          func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "" },
		"ref length": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ProviderRef = strings.Repeat("r", 256)
		},
		"ref control":      func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "ref\x00" },
		"ref invalid UTF8": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "ref\xff" },
		"image mismatch": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ImageDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"invalid image": func(s *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.ImageDigest, a.ImageDigest = "latest", "latest"
		},
		"legacy observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ObservedControlSessionID = "" },
		"wrong observation session": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ObservedControlSessionID = bindingSessionTwo
		},
		"missing observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.LastObservedAt = nil },
		"stale observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(-45 * time.Second)
			a.LastObservedAt = &at
		},
		"future observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(time.Nanosecond)
			a.LastObservedAt = &at
		},
		"future assignment": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) {
			a.AssignedAt = now.Add(time.Second)
		},
		"node identity":     func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.ID = "another-node" },
		"disconnected node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.Status = "disconnected" },
		"empty session":     func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.ControlSessionID = "" },
		"invalid session": func(_ *Slot, a *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID, a.ObservedControlSessionID = "session-old", "session-old"
		},
		"uppercase session": func(_ *Slot, a *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID, a.ObservedControlSessionID = strings.Repeat("A", 32), strings.Repeat("A", 32)
		},
		"nonhex session": func(_ *Slot, a *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID, a.ObservedControlSessionID = strings.Repeat("g", 32), strings.Repeat("g", 32)
		},
		"padded session": func(_ *Slot, a *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID, a.ObservedControlSessionID = bindingSessionOne+" ", bindingSessionOne+" "
		},
		"missing node time": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.LastSeenAt = nil },
		"stale node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(-45 * time.Second)
			n.LastSeenAt = &at
		},
		"future node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(time.Nanosecond)
			n.LastSeenAt = &at
		},
		"lease slot":    func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.SlotID = "another-slot" },
		"lease node":    func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.NodeID = "another-node" },
		"lease epoch":   func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.ExecutionEpoch++ },
		"empty owner":   func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.OwnerID = "" },
		"invalid owner": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.OwnerID = "owner\n" },
		"revoked lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) { l.RevokedAt = &now },
		"expired lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) { l.ExpiresAt = now },
		"future lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) {
			l.CreatedAt = now.Add(time.Second)
		},
		"future lease update": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) {
			l.UpdatedAt = now.Add(time.Second)
		},
		"lease update before create": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) {
			l.UpdatedAt = l.CreatedAt.Add(-time.Second)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, now := executionBindingFixture(t)
			r.mu.Lock()
			slot, assignment, node, lease := r.slots["slot-1"], r.assignments["slot-1"][0], r.nodes["srv74"], r.executionLeases[executionLeaseKey("slot-1", 1)]
			change(&slot, &assignment, &node, &lease, now)
			r.slots["slot-1"], r.assignments["slot-1"][0], r.nodes["srv74"], r.executionLeases[executionLeaseKey("slot-1", 1)] = slot, assignment, node, lease
			r.mu.Unlock()
			got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireBindingDenied(t, got, err)
		})
	}
}

func TestReadExecutionBindingMemoryMissingAndDuplicateRows(t *testing.T) {
	for _, kind := range []string{"slot", "assignment", "node", "lease", "duplicate active"} {
		t.Run(kind, func(t *testing.T) {
			r, now := executionBindingFixture(t)
			r.mu.Lock()
			switch kind {
			case "slot":
				delete(r.slots, "slot-1")
			case "assignment":
				delete(r.assignments, "slot-1")
			case "node":
				delete(r.nodes, "srv74")
			case "lease":
				delete(r.executionLeases, executionLeaseKey("slot-1", 1))
			case "duplicate active":
				r.assignments["slot-1"] = append(r.assignments["slot-1"], r.assignments["slot-1"][0])
			}
			r.mu.Unlock()
			got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireBindingDenied(t, got, err)
		})
	}
}

func TestReadExecutionBindingMemoryReadIsAtomic(t *testing.T) {
	r, now := executionBindingFixture(t)
	var group sync.WaitGroup
	group.Add(4)
	go func() {
		defer group.Done()
		for i := range 1000 {
			r.mu.Lock()
			slot, lease := r.slots["slot-1"], r.executionLeases[executionLeaseKey("slot-1", 1)]
			suffix := fmt.Sprint(i % 2)
			slot.AccountID, lease.OwnerID = "account-"+suffix, "owner-"+suffix
			r.slots["slot-1"], r.executionLeases[executionLeaseKey("slot-1", 1)] = slot, lease
			r.mu.Unlock()
		}
	}()
	for range 3 {
		go func() {
			defer group.Done()
			for range 300 {
				got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
				if err != nil || strings.TrimPrefix(got.AccountID, "account-") != strings.TrimPrefix(got.LeaseOwnerID, "owner-") {
					t.Errorf("read mixed concurrent versions: %+v / %v", got, err)
					return
				}
			}
		}()
	}
	group.Wait()
}

func bindingSQLMatcher(_ string, actual string) error {
	normalized := strings.Join(strings.Fields(actual), " ")
	for _, guard := range []string{
		"SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id",
		"n.control_session_id, sa.image_digest, sa.execution_epoch, sa.desired_generation",
		"sa.last_observed_at, n.last_seen_at, el.expires_at", "sa.assigned_at, el.created_at, el.updated_at",
		"FROM slots s", "JOIN slot_assignments sa ON sa.slot_id = s.slot_id",
		"BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL",
		"JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id",
		"JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id",
		"el.execution_epoch = sa.execution_epoch", "el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id", "WHERE s.slot_id = ?",
		"s.desired_state = 'ready'", "s.desired_generation > 0 AND sa.desired_generation = s.desired_generation",
		"sa.execution_epoch > 0", "BINARY sa.image_digest = BINARY s.image_digest",
		"sa.healthy = 1 AND sa.actual_state IN ('ready', 'running', 'busy')",
		"sa.provider_ref IS NOT NULL AND sa.provider_ref <> ''", "n.status = 'connected'",
		"n.control_session_id IS NOT NULL AND n.control_session_id <> ''", "sa.observed_control_session_id IS NOT NULL",
		"BINARY sa.observed_control_session_id = BINARY n.control_session_id",
		"sa.last_observed_at > ? AND sa.last_observed_at <= ?", "n.last_seen_at > ? AND n.last_seen_at <= ?",
		"sa.assigned_at <= ?", "el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ?", "el.expires_at > ?",
	} {
		if !strings.Contains(normalized, guard) {
			return fmt.Errorf("missing execution-binding query guard: %s", guard)
		}
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|DELETE|REPLACE|FOR\s+UPDATE)\b|credential|route_cache`).MatchString(normalized) || strings.Count(normalized, "SELECT ") != 1 {
		return errors.New("query must be one read-only authority projection")
	}
	return nil
}

var bindingColumns = []string{"account_id", "slot_id", "node_id", "provider_ref", "owner_id", "control_session_id", "image_digest",
	"execution_epoch", "desired_generation", "last_observed_at", "last_seen_at", "expires_at", "assigned_at", "created_at", "updated_at"}

func bindingRow(now time.Time) []driver.Value {
	return []driver.Value{"account-1", "slot-1", "srv74", "fake://slot-1", "owner-1", bindingSessionOne, "sha256:" + strings.Repeat("a", 64),
		int64(3), int64(7), now.Add(-time.Second), now.Add(-2 * time.Second), now.Add(30 * time.Second), now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Second)}
}

func bindingSQLFixture(t *testing.T) (*Repository, sqlmock.Sqlmock, time.Time) {
	t.Helper()
	db, mocked, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(bindingSQLMatcher)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, _ := NewRepository(db)
	return r, mocked, time.Unix(2_000_000_000, 0).UTC()
}

func expectBindingRead(mocked sqlmock.Sqlmock, now time.Time) *sqlmock.ExpectedQuery {
	cutoff := now.Add(-45 * time.Second)
	return mocked.ExpectQuery("execution-binding authority contract").WithArgs("slot-1", cutoff, now, cutoff, now, now, now, now, now)
}

func TestReadExecutionBindingSQLSingleReadContract(t *testing.T) {
	r, mocked, now := bindingSQLFixture(t)
	expectBindingRead(mocked, now).WillReturnRows(sqlmock.NewRows(bindingColumns).AddRow(bindingRow(now)...))
	got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || got.AccountID != "account-1" || got.SlotID != "slot-1" || got.ExecutionEpoch != 3 || got.RouteGeneration != 7 ||
		got.ControlSessionID != bindingSessionOne || !got.ObservedAt.Equal(now.Add(-time.Second)) || !got.NodeSeenAt.Equal(now.Add(-2*time.Second)) || !got.LeaseExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("unexpected SQL binding: %+v / %v", got, err)
	}
	if err := mocked.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReadExecutionBindingSQLRejectsErrorsAndBadProjection(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		value driver.Value
	}{
		{"empty account", 0, ""}, {"wrong slot", 1, "another-slot"}, {"bad node", 2, "bad\nnode"},
		{"large ref", 3, strings.Repeat("r", 256)}, {"empty owner", 4, ""}, {"null session", 5, nil}, {"invalid session", 5, "session-1"},
		{"uppercase session", 5, strings.Repeat("A", 32)}, {"nonhex session", 5, strings.Repeat("g", 32)}, {"padded session", 5, bindingSessionOne + " "},
		{"bad image", 6, "latest"}, {"zero epoch", 7, int64(0)}, {"zero generation", 8, int64(0)},
		{"null observation", 9, nil}, {"missing node time", 10, nil}, {"zero lease expiry", 11, time.Time{}},
		{"zero assignment time", 12, time.Time{}}, {"zero lease creation", 13, time.Time{}}, {"zero lease update", 14, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, mocked, now := bindingSQLFixture(t)
			row := bindingRow(now)
			row[test.index] = test.value
			expectBindingRead(mocked, now).WillReturnRows(sqlmock.NewRows(bindingColumns).AddRow(row...))
			got, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireBindingDenied(t, got, err)
			if err := mocked.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, failure := range []string{"storage", "missing", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			r, mocked, now := bindingSQLFixture(t)
			query := expectBindingRead(mocked, now)
			ctx := context.Background()
			switch failure {
			case "storage":
				query.WillReturnError(errors.New("synthetic private database error"))
			case "missing":
				query.WillReturnRows(sqlmock.NewRows(bindingColumns))
			case "cancellation":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
				defer cancel()
				query.WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows(bindingColumns).AddRow(bindingRow(now)...))
			}
			got, err := r.ReadExecutionBinding(ctx, "slot-1", now, 45*time.Second)
			requireBindingDenied(t, got, err)
			if err := mocked.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadExecutionBindingInvalidArgumentsNeverQuery(t *testing.T) {
	r, mocked, now := bindingSQLFixture(t)
	memory, _ := executionBindingFixture(t)
	for _, implementation := range []ExecutionBindingRepository{r, memory, (*Repository)(nil), (*MemoryRepository)(nil), &Repository{}} {
		for _, test := range []struct {
			ctx    context.Context
			slotID string
			at     time.Time
			age    time.Duration
		}{
			{nil, "slot-1", now, time.Second}, {context.Background(), "", now, time.Second},
			{context.Background(), "slot\n", now, time.Second}, {context.Background(), "slot-1", time.Time{}, time.Second},
			{context.Background(), "slot-1", now, 0}, {context.Background(), "slot-1", now, -time.Second},
			{context.Background(), "slot-1", now, 45*time.Second + time.Nanosecond},
		} {
			got, err := implementation.ReadExecutionBinding(test.ctx, test.slotID, test.at, test.age)
			requireBindingDenied(t, got, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := implementation.ReadExecutionBinding(ctx, "slot-1", now, time.Second)
		requireBindingDenied(t, got, err)
	}
	if err := mocked.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
