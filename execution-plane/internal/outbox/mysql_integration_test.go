package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLSourceIntegration(t *testing.T) {
	dsn := os.Getenv("EXECUTION_CCMAX_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set EXECUTION_CCMAX_MYSQL_TEST_DSN to run the CCMAX outbox source integration")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	eventID := fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(suffix), uint64(suffix)&0xffffffffffff)
	consumerName := fmt.Sprintf("execution-integration-%d", suffix)
	result, err := db.ExecContext(ctx, `INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES (?, ?, 'account.runtime.provision_requested', 1, '{"provider":"docker"}')`, eventID, suffix)
	if err != nil {
		t.Fatal(err)
	}
	sequence, _ := result.LastInsertId()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM runtime_outbox_consumers WHERE consumer_name = ?`, consumerName)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM runtime_outbox WHERE event_id = ?`, eventID)
	})
	source, err := NewMySQLSource(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := source.Claim(ctx, consumerName, "replica-a", now, time.Minute)
	if err != nil || !ok || claimed.Sequence != sequence || claimed.EventID != eventID {
		t.Fatalf("MySQL source claim: event=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := source.Nack(ctx, consumerName, "replica-a", sequence, "integration_retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = source.Claim(ctx, consumerName, "replica-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.EventID != eventID {
		t.Fatalf("MySQL source replay: event=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := source.Ack(ctx, consumerName, "replica-b", sequence, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := source.Claim(ctx, consumerName, "replica-c", now.Add(4*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("MySQL source empty claim: ok=%v err=%v", ok, err)
	}
}
