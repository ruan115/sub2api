package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLAccountRuntimeSourceLoadsStableIdentityForOrderedHistoricalEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defaults := CCMAXRuntimeDefaults{
		RequiredLabels: map[string]string{"region": "ap-shanghai"},
		ImageDigest:    "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 256 << 20,
	}
	source, err := NewMySQLAccountRuntimeSource(db, defaults)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`(?s)SELECT runtime_generation, runtime_slot_id, runtime_provider.*FROM accounts.*WHERE id = \?`).
		WithArgs(int64(10380)).WillReturnRows(sqlmock.NewRows([]string{
		"runtime_generation", "runtime_slot_id", "runtime_provider",
	}).AddRow(9, "ccmax-account-10380", "docker"))
	desired, err := source.LoadAccountRuntimeDesired(context.Background(), 10380, 7)
	if err != nil || desired.DesiredGeneration != 7 || desired.SlotID != "ccmax-account-10380" ||
		desired.RequiredLabels["region"] != "ap-shanghai" {
		t.Fatalf("desired = %+v, %v", desired, err)
	}
	desired.RequiredLabels["region"] = "mutated"
	if defaults.RequiredLabels["region"] != "ap-shanghai" || source.defaults.RequiredLabels["region"] != "ap-shanghai" {
		t.Fatal("runtime labels were not cloned")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAccountRuntimeSourceRejectsUntrustedOrFutureIdentity(t *testing.T) {
	defaults := CCMAXRuntimeDefaults{
		ImageDigest: "sha256:" + strings.Repeat("b", 64), CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
	}
	for _, test := range []struct {
		name       string
		generation uint64
		slotID     string
		provider   string
	}{
		{name: "future generation", generation: 6, slotID: "ccmax-account-7", provider: "docker"},
		{name: "foreign slot", generation: 7, slotID: "slot-user-controlled", provider: "docker"},
		{name: "foreign provider", generation: 7, slotID: "ccmax-account-7", provider: "virtual-machine"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			source, _ := NewMySQLAccountRuntimeSource(db, defaults)
			mock.ExpectQuery(`(?s)SELECT runtime_generation, runtime_slot_id, runtime_provider.*FROM accounts.*WHERE id = \?`).
				WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{
				"runtime_generation", "runtime_slot_id", "runtime_provider",
			}).AddRow(test.generation, test.slotID, test.provider))
			if _, err := source.LoadAccountRuntimeDesired(context.Background(), 7, 7); !errors.Is(err, ErrCCMAXRuntimeDesired) {
				t.Fatalf("identity error = %v", err)
			}
		})
	}
	if _, err := NewMySQLAccountRuntimeSource(nil, defaults); !errors.Is(err, ErrCCMAXRuntimeDesired) {
		t.Fatalf("nil database error = %v", err)
	}
	invalid := defaults
	invalid.ImageDigest = "latest"
	db, _, _ := sqlmock.New()
	defer db.Close()
	if _, err := NewMySQLAccountRuntimeSource(db, invalid); !errors.Is(err, ErrCCMAXRuntimeDesired) {
		t.Fatalf("invalid defaults error = %v", err)
	}
}
