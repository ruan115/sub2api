package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListPublishableRoutesRequiresLiveLeaseAndEndpointLabel(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	labels, _ := json.Marshal(map[string]string{"dataplane_endpoint": "10.8.0.12:8091", "region": "ap-shanghai"})
	missing, _ := json.Marshal(map[string]string{"region": "ap-shanghai"})
	rows := sqlmock.NewRows([]string{"slot_id", "node_id", "labels_json", "execution_epoch", "desired_generation"}).
		AddRow("slot-ready", "srv74", labels, 3, 7).
		AddRow("slot-no-endpoint", "srv75", missing, 3, 7)
	mock.ExpectQuery(`(?s)SELECT sa.slot_id, sa.node_id, n.labels_json, sa.execution_epoch, sa.desired_generation.*FROM slot_assignments sa.*execution_leases el.*sa.desired_generation = s.desired_generation.*s.desired_state = 'ready'`).
		WithArgs(now, now, now).
		WillReturnRows(rows)
	routes, err := repository.ListPublishableRoutes(context.Background(), now)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
	if routes[0].SlotID != "slot-ready" || routes[0].Endpoint != "10.8.0.12:8091" || routes[0].Epoch != 3 || routes[0].Generation != 7 {
		t.Fatalf("route = %+v", routes[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
