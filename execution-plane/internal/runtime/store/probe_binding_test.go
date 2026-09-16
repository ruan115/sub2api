package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func requireProbeDenied(t *testing.T, got ProbeBinding, err error) {
	t.Helper()
	if !reflect.DeepEqual(got, ProbeBinding{}) || err != ErrProbeBindingUnavailable {
		t.Fatalf("probe = %+v / %v, want empty denied result", got, err)
	}
}

func requireProbePageDenied(t *testing.T, got ProbeBindingPage, err error) {
	t.Helper()
	if got.Bindings != nil || got.NextAfterSlotID != "" || err != ErrProbeBindingUnavailable {
		t.Fatalf("page = %+v / %v, want empty denied result", got, err)
	}
}

func TestProbeBindingAllowsUnprovenObservationsWithoutPromotingAuthority(t *testing.T) {
	for _, kind := range []string{"missing", "legacy", "stale", "old session", "unhealthy"} {
		t.Run(kind, func(t *testing.T) {
			r, now := executionBindingFixture(t)
			r.mu.Lock()
			a := &r.assignments["slot-1"][0]
			switch kind {
			case "missing":
				a.LastObservedAt, a.ObservedControlSessionID = nil, ""
			case "legacy":
				a.ObservedControlSessionID = ""
			case "stale":
				at := now.Add(-time.Hour)
				a.LastObservedAt = &at
			case "old session":
				a.ObservedControlSessionID = bindingSessionTwo
			case "unhealthy":
				a.Healthy, a.ActualState = false, "failed"
			}
			r.mu.Unlock()
			got, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
			if err != nil || got.ControlSessionID != bindingSessionOne || got.ImageDigest != "sha256:"+strings.Repeat("a", 64) ||
				got.AccountID != "account-1" || got.ProviderRef != "fake://slot-1" || got.LeaseOwnerID != "owner-1" {
				t.Fatalf("probe = %+v / %v", got, err)
			}
			business, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireBindingDenied(t, business, err)
			page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 2)
			if err != nil || len(page.Bindings) != 1 || page.NextAfterSlotID != "" || !reflect.DeepEqual(page.Bindings[0], got) {
				t.Fatalf("page = %+v / %v", page, err)
			}
		})
	}
}

func TestProbeBindingReconnectCanBeInspectedButNotExecuted(t *testing.T) {
	r, now := executionBindingFixture(t)
	node, _ := r.GetNode(context.Background(), "srv74")
	if err := r.MarkDisconnected(context.Background(), node.ID, bindingSessionOne, now); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
	requireProbeDenied(t, got, err)
	if err := r.AcceptHello(context.Background(), Hello{NodeID: node.ID, SessionID: bindingSessionTwo,
		ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || got.ControlSessionID != bindingSessionTwo || got.ObservedControlSessionID != bindingSessionOne ||
		got.LastObservedAt == nil || !got.LastObservedAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("reconnect probe = %+v / %v", got, err)
	}
	business, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	requireBindingDenied(t, business, err)
	*got.LastObservedAt = now.Add(time.Hour)
	page, err := r.ListProbeBindings(context.Background(), "", now.Add(time.Second), 45*time.Second, 1)
	if err != nil || len(page.Bindings) != 1 || !page.Bindings[0].LastObservedAt.Equal(now.Add(-time.Second)) {
		t.Fatal("read exposed or refreshed the observation")
	}
	*page.Bindings[0].LastObservedAt = now
	next, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || !next.LastObservedAt.Equal(now.Add(-time.Second)) {
		t.Fatal("page exposed a mutable observation pointer")
	}
}

func TestProbeBindingRejectsInconsistentAndExpiredRows(t *testing.T) {
	changes := map[string]func(*Slot, *Assignment, *Node, *ExecutionLease, time.Time){
		"slot identity": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.ID = "other" },
		"account invalid": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.AccountID = "private\naccount"
		},
		"account too long": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.AccountID = strings.Repeat("a", 129)
		},
		"desired absent": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredState = "absent" },
		"zero generation": func(s *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.DesiredGeneration, a.DesiredGeneration = 0, 0
		},
		"generation changed": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredGeneration++ },
		"assignment slot":    func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.SlotID = "other" },
		"assignment node":    func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.NodeID = "other" },
		"epoch changed":      func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ExecutionEpoch++ },
		"released":           func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) { a.ReleasedAt = &now },
		"empty ref":          func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "" },
		"ref length": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ProviderRef = strings.Repeat("r", 256)
		},
		"ref control": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "ref\x00" },
		"ref UTF8":    func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ProviderRef = "ref\xff" },
		"image changed": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ImageDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"image invalid": func(s *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.ImageDigest, a.ImageDigest = "latest", "latest"
		},
		"future observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(time.Nanosecond)
			a.LastObservedAt = &at
		},
		"zero observation": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.LastObservedAt = &time.Time{} },
		"bad observed session": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			a.ObservedControlSessionID = "bad-session"
		},
		"future assignment": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, now time.Time) {
			a.AssignedAt = now.Add(time.Nanosecond)
		},
		"zero assignment time": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.AssignedAt = time.Time{} },
		"node identity":        func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.ID = "other" },
		"disconnected":         func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.Status = "disconnected" },
		"empty session":        func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.ControlSessionID = "" },
		"invalid session": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID = strings.Repeat("g", 32)
		},
		"uppercase session": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) {
			n.ControlSessionID = strings.Repeat("A", 32)
		},
		"missing node time": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.LastSeenAt = nil },
		"stale node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, now time.Time) {
			at := now.Add(-45 * time.Second)
			n.LastSeenAt = &at
		},
		"future node": func(_ *Slot, _ *Assignment, n *Node, _ExecutionLease *ExecutionLease, now time.Time) {
			at := now.Add(time.Nanosecond)
			n.LastSeenAt = &at
		},
		"lease slot":    func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.SlotID = "other" },
		"lease node":    func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.NodeID = "other" },
		"lease epoch":   func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.ExecutionEpoch++ },
		"empty owner":   func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.OwnerID = "" },
		"revoked lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) { l.RevokedAt = &now },
		"expired lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) { l.ExpiresAt = now },
		"future lease create": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) {
			l.CreatedAt = now.Add(time.Second)
		},
		"future lease update": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, now time.Time) {
			l.UpdatedAt = now.Add(time.Second)
		},
		"zero lease create": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.CreatedAt = time.Time{} },
		"lease history reversed": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) {
			l.UpdatedAt = l.CreatedAt.Add(-time.Second)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, now := executionBindingFixture(t)
			r.mu.Lock()
			s, a, n, l := r.slots["slot-1"], r.assignments["slot-1"][0], r.nodes["srv74"], r.executionLeases[executionLeaseKey("slot-1", 1)]
			change(&s, &a, &n, &l, now)
			r.slots["slot-1"], r.assignments["slot-1"][0], r.nodes["srv74"], r.executionLeases[executionLeaseKey("slot-1", 1)] = s, a, n, l
			r.mu.Unlock()
			got, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireProbeDenied(t, got, err)
			page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 2)
			if err != nil || len(page.Bindings) != 0 || page.NextAfterSlotID != "" {
				t.Fatalf("invalid row returned in page: %+v / %v", page, err)
			}
		})
	}
}

func probeMemoryPageFixture(t *testing.T, ids ...string) (*MemoryRepository, time.Time) {
	t.Helper()
	r, now := executionBindingFixture(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	s, a, l := r.slots["slot-1"], r.assignments["slot-1"][0], r.executionLeases[executionLeaseKey("slot-1", 1)]
	delete(r.slots, "slot-1")
	delete(r.assignments, "slot-1")
	delete(r.executionLeases, executionLeaseKey("slot-1", 1))
	for _, id := range ids {
		s.ID, a.SlotID, l.SlotID = id, id, id
		r.slots[id], r.assignments[id], r.executionLeases[executionLeaseKey(id, 1)] = s, []Assignment{a}, l
	}
	return r, now
}

func TestProbeBindingMemoryBinaryKeysetAndFilteredPageProgress(t *testing.T) {
	r, now := probeMemoryPageFixture(t, "slot-a", "slot-A", "slot-z", "slot-Z", "slot-中", "slot-0-expired", "slot-0-unassigned")
	r.mu.Lock()
	for _, id := range []string{"slot-A", "slot-Z"} {
		s := r.slots[id]
		s.AccountID = "invalid\naccount"
		r.slots[id] = s
	}
	l := r.executionLeases[executionLeaseKey("slot-0-expired", 1)]
	l.ExpiresAt = now
	r.executionLeases[executionLeaseKey("slot-0-expired", 1)] = l
	delete(r.assignments, "slot-0-unassigned")
	r.mu.Unlock()
	page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 2)
	if err != nil || len(page.Bindings) != 0 || page.NextAfterSlotID != "slot-Z" {
		t.Fatalf("empty filtered page did not advance: %+v / %v", page, err)
	}
	page, err = r.ListProbeBindings(context.Background(), page.NextAfterSlotID, now, 45*time.Second, 2)
	if err != nil || len(page.Bindings) != 2 || page.Bindings[0].SlotID != "slot-a" || page.Bindings[1].SlotID != "slot-z" || page.NextAfterSlotID != "slot-z" {
		t.Fatalf("binary middle page = %+v / %v", page, err)
	}
	page, err = r.ListProbeBindings(context.Background(), page.NextAfterSlotID, now, 45*time.Second, 2)
	if err != nil || len(page.Bindings) != 1 || page.Bindings[0].SlotID != "slot-中" || page.NextAfterSlotID != "" {
		t.Fatalf("terminal page = %+v / %v", page, err)
	}
	page, err = r.ListProbeBindings(context.Background(), "slot-中", now, 45*time.Second, 2)
	if err != nil || page.Bindings == nil || len(page.Bindings) != 0 || page.NextAfterSlotID != "" {
		t.Fatalf("empty terminal page = %+v / %v", page, err)
	}
}

func TestProbeBindingMemoryPageBoundAndInvalidLastCursor(t *testing.T) {
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("slot-%03d", i)
	}
	r, now := probeMemoryPageFixture(t, ids...)
	page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 100)
	if err != nil || len(page.Bindings) != 100 || page.Bindings[0].SlotID != "slot-000" || page.NextAfterSlotID != "slot-099" {
		t.Fatalf("bounded page = %+v / %v", page, err)
	}
	r, now = probeMemoryPageFixture(t, "slot-A", "slot-Z\n")
	page, err = r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 2)
	requireProbePageDenied(t, page, err)
}

func TestProbeBindingMemoryAtomicReadAndCancellation(t *testing.T) {
	r, now := executionBindingFixture(t)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for i := range 500 {
			r.mu.Lock()
			s, l := r.slots["slot-1"], r.executionLeases[executionLeaseKey("slot-1", 1)]
			s.AccountID, l.OwnerID = fmt.Sprintf("account-%d", i%2), fmt.Sprintf("owner-%d", i%2)
			r.slots["slot-1"], r.executionLeases[executionLeaseKey("slot-1", 1)] = s, l
			r.mu.Unlock()
		}
	}()
	go func() {
		defer group.Done()
		for range 300 {
			page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 1)
			if err != nil || len(page.Bindings) != 1 {
				t.Error("concurrent page failed")
				return
			}
			got := page.Bindings[0]
			if strings.TrimPrefix(got.AccountID, "account-") != strings.TrimPrefix(got.LeaseOwnerID, "owner-") {
				t.Error("page mixed independently locked versions")
				return
			}
		}
	}()
	group.Wait()
	for _, list := range []bool{false, true} {
		parent, cancel := context.WithCancel(context.Background())
		ctx := &firstErrObservedContext{Context: parent, observed: make(chan struct{})}
		finished := make(chan error, 1)
		r.mu.Lock()
		go func() {
			if list {
				_, err := r.ListProbeBindings(ctx, "", now, 45*time.Second, 1)
				finished <- err
			} else {
				_, err := r.ReadProbeBinding(ctx, "slot-1", now, 45*time.Second)
				finished <- err
			}
		}()
		select {
		case <-ctx.observed:
		case <-time.After(time.Second):
			r.mu.Unlock()
			cancel()
			t.Fatal("read did not validate arguments")
		}
		cancel()
		r.mu.Unlock()
		select {
		case err := <-finished:
			if err != ErrProbeBindingUnavailable {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled read did not finish")
		}
	}
}

var probeColumns = []string{"account_id", "slot_id", "node_id", "provider_ref", "owner_id", "control_session_id", "image_digest",
	"execution_epoch", "desired_generation", "last_seen_at", "expires_at", "last_observed_at", "observed_control_session_id",
	"assigned_at", "created_at", "updated_at"}

func probeRow(now time.Time, slotID string) []driver.Value {
	return []driver.Value{"account-1", slotID, "srv74", "fake://slot-1", "owner-1", bindingSessionOne, "sha256:" + strings.Repeat("a", 64),
		int64(1), int64(1), now.Add(-time.Second), now.Add(time.Minute), nil, nil, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Second)}
}

func probeSQLMatcher(expected, actual string) error {
	normalized := strings.Join(strings.Fields(actual), " ")
	for _, guard := range []string{
		"SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id",
		"n.last_seen_at, el.expires_at, sa.last_observed_at, sa.observed_control_session_id",
		"FROM slots s", "JOIN slot_assignments sa ON sa.slot_id = s.slot_id", "BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL",
		"JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id",
		"JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id",
		"el.execution_epoch = sa.execution_epoch", "el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id",
		"s.desired_state = 'ready'", "s.desired_generation > 0 AND sa.desired_generation = s.desired_generation",
		"sa.execution_epoch > 0 AND BINARY sa.image_digest = BINARY s.image_digest",
		"sa.provider_ref IS NOT NULL AND sa.provider_ref <> ''", "n.status = 'connected'", "n.control_session_id IS NOT NULL AND n.control_session_id <> ''",
		"n.last_seen_at > ? AND n.last_seen_at <= ?", "(sa.last_observed_at IS NULL OR sa.last_observed_at <= ?)",
		"sa.assigned_at <= ?", "el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ?", "el.expires_at > ?",
	} {
		if !strings.Contains(normalized, guard) {
			return fmt.Errorf("missing probe query guard %s", guard)
		}
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|DELETE|REPLACE)\b|credential|route_cache|sa\.healthy|sa\.actual_state|sa\.last_observed_at >|BINARY sa\.observed_control_session_id`).MatchString(normalized) || strings.Count(normalized, "SELECT ") != 1 {
		return errors.New("probe query reads secrets, writes state or incorrectly requires prior readiness")
	}
	if expected == "list" {
		if !strings.HasSuffix(normalized, "AND BINARY s.slot_id > BINARY ? ORDER BY BINARY s.slot_id LIMIT ?") {
			return errors.New("list is not a bounded binary keyset query")
		}
	} else if !strings.HasSuffix(normalized, "AND s.slot_id = ?") {
		return errors.New("read is not scoped to one slot")
	}
	return nil
}

func probeSQLFixture(t *testing.T) (*Repository, sqlmock.Sqlmock, time.Time) {
	t.Helper()
	db, mocked, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(probeSQLMatcher)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, _ := NewRepository(db)
	return r, mocked, time.Unix(2_000_000_000, 0).UTC()
}

func expectProbeQuery(mocked sqlmock.Sqlmock, now time.Time, list bool, cursor string, limit int) *sqlmock.ExpectedQuery {
	args := []driver.Value{now.Add(-45 * time.Second), now, now, now, now, now, now, cursor}
	if list {
		return mocked.ExpectQuery("list").WithArgs(append(args, limit)...)
	}
	return mocked.ExpectQuery("read").WithArgs(args...)
}

func TestProbeBindingSQLContractAndObservationPreservation(t *testing.T) {
	for _, observed := range []driver.Value{nil, time.Unix(1_900_000_000, 0).UTC()} {
		r, mocked, now := probeSQLFixture(t)
		row := probeRow(now, "slot-1")
		row[11], row[12] = observed, bindingSessionTwo
		expectProbeQuery(mocked, now, false, "slot-1", 0).WillReturnRows(sqlmock.NewRows(probeColumns).AddRow(row...))
		got, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
		if err != nil || got.ControlSessionID != bindingSessionOne || got.ObservedControlSessionID != bindingSessionTwo || (got.LastObservedAt == nil) != (observed == nil) {
			t.Fatalf("SQL probe = %+v / %v", got, err)
		}
		if observed != nil && !got.LastObservedAt.Equal(observed.(time.Time)) {
			t.Fatal("query refreshed observation")
		}
		if err := mocked.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeBindingSQLFilteredEmptyPageStillAdvances(t *testing.T) {
	r, mocked, now := probeSQLFixture(t)
	bad := probeRow(now, "slot-A")
	bad[0] = "bad\naccount"
	expectProbeQuery(mocked, now, true, "", 1).WillReturnRows(sqlmock.NewRows(probeColumns).AddRow(bad...)).RowsWillBeClosed()
	page, err := r.ListProbeBindings(context.Background(), "", now, 45*time.Second, 1)
	if err != nil || len(page.Bindings) != 0 || page.NextAfterSlotID != "slot-A" {
		t.Fatalf("page=%+v / %v", page, err)
	}
	expectProbeQuery(mocked, now, true, page.NextAfterSlotID, 2).WillReturnRows(sqlmock.NewRows(probeColumns).AddRow(probeRow(now, "slot-B")...)).RowsWillBeClosed()
	page, err = r.ListProbeBindings(context.Background(), page.NextAfterSlotID, now, 45*time.Second, 2)
	if err != nil || len(page.Bindings) != 1 || page.Bindings[0].SlotID != "slot-B" || page.NextAfterSlotID != "" {
		t.Fatalf("terminal page=%+v / %v", page, err)
	}
	if err := mocked.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeBindingSQLReadRejectsMalformedProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		column int
		value  driver.Value
	}{
		{"wrong slot", 1, "other"}, {"bad account", 0, "bad\n"}, {"bad node", 2, ""}, {"bad ref", 3, " padded "}, {"bad owner", 4, ""},
		{"bad session", 5, strings.Repeat("g", 32)}, {"bad image", 6, "latest"}, {"zero epoch", 7, int64(0)}, {"zero generation", 8, int64(0)},
		{"missing node time", 9, nil}, {"expired lease", 10, time.Time{}}, {"zero observation", 11, time.Time{}}, {"invalid observed session", 12, "invalid"},
		{"missing assignment", 13, time.Time{}}, {"missing lease history", 14, time.Time{}}, {"invalid lease history", 15, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, mocked, now := probeSQLFixture(t)
			row := probeRow(now, "slot-1")
			row[test.column] = test.value
			expectProbeQuery(mocked, now, false, "slot-1", 0).WillReturnRows(sqlmock.NewRows(probeColumns).AddRow(row...))
			got, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireProbeDenied(t, got, err)
			if err := mocked.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProbeBindingSQLFailuresHaveNoPartialPages(t *testing.T) {
	for _, kind := range []string{"query error", "row error", "scan error", "unordered", "duplicate", "old cursor", "bad cursor", "too many rows", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r, mocked, now := probeSQLFixture(t)
			query := expectProbeQuery(mocked, now, true, "slot-0", 2)
			rows := sqlmock.NewRows(probeColumns).AddRow(probeRow(now, "slot-A")...)
			ctx := context.Background()
			switch kind {
			case "query error":
				query.WillReturnError(errors.New("private storage detail"))
			case "row error":
				query.WillReturnRows(rows.AddRow(probeRow(now, "slot-B")...).RowError(1, errors.New("private row detail")))
			case "scan error":
				row := probeRow(now, "slot-B")
				row[7] = "not-an-epoch"
				query.WillReturnRows(rows.AddRow(row...))
			case "unordered":
				query.WillReturnRows(rows.AddRow(probeRow(now, "slot-9")...))
			case "duplicate":
				query.WillReturnRows(rows.AddRow(probeRow(now, "slot-A")...))
			case "old cursor":
				query.WillReturnRows(sqlmock.NewRows(probeColumns).AddRow(probeRow(now, "slot-0")...))
			case "bad cursor":
				query.WillReturnRows(rows.AddRow(probeRow(now, "slot-Z\n")...))
			case "too many rows":
				query.WillReturnRows(rows.AddRow(probeRow(now, "slot-B")...).AddRow(probeRow(now, "slot-C")...))
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
				defer cancel()
				query.WillDelayFor(time.Second).WillReturnRows(rows)
			}
			got, err := r.ListProbeBindings(ctx, "slot-0", now, 45*time.Second, 2)
			requireProbePageDenied(t, got, err)
			if err := mocked.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProbeBindingInvalidArgumentsNeverQuery(t *testing.T) {
	r, mocked, now := probeSQLFixture(t)
	memory, _ := executionBindingFixture(t)
	for _, repository := range []ProbeBindingRepository{r, memory, (*Repository)(nil), (*MemoryRepository)(nil), &Repository{}} {
		for _, test := range []struct {
			ctx   context.Context
			key   string
			at    time.Time
			age   time.Duration
			limit int
		}{
			{nil, "slot-1", now, time.Second, 1}, {context.Background(), "bad\n", now, time.Second, 1},
			{context.Background(), "slot-1", time.Time{}, time.Second, 1}, {context.Background(), "slot-1", now, 0, 1},
			{context.Background(), "slot-1", now, -time.Second, 1}, {context.Background(), "slot-1", now, 45*time.Second + 1, 1},
		} {
			got, err := repository.ReadProbeBinding(test.ctx, test.key, test.at, test.age)
			requireProbeDenied(t, got, err)
			page, err := repository.ListProbeBindings(test.ctx, test.key, test.at, test.age, test.limit)
			requireProbePageDenied(t, page, err)
		}
		for _, limit := range []int{-1, 0, 101} {
			page, err := repository.ListProbeBindings(context.Background(), "", now, time.Second, limit)
			requireProbePageDenied(t, page, err)
		}
		got, err := repository.ReadProbeBinding(context.Background(), "", now, time.Second)
		requireProbeDenied(t, got, err)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err = repository.ReadProbeBinding(ctx, "slot-1", now, time.Second)
		requireProbeDenied(t, got, err)
		page, err := repository.ListProbeBindings(ctx, "", now, time.Second, 1)
		requireProbePageDenied(t, page, err)
	}
	if err := mocked.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
