package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	enrollment "github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func receiptFixture(t *testing.T) (enrollment.Receipt, func(*ecdsa.PrivateKey) enrollment.Receipt, *ecdsa.PrivateKey) {
	t.Helper()
	now := time.Now().UTC()
	a, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 3, Generation: 7}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makeReceipt := func(k *ecdsa.PrivateKey) enrollment.Receipt {
		u, _ := binding.URI()
		der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}}, k)
		if e != nil {
			t.Fatal(e)
		}
		v, e := a.IssueRuntime(binding, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
		if e != nil {
			t.Fatal(e)
		}
		return enrollment.Receipt{AssignmentID: "assignment-a", Binding: binding, PublicKeySHA256: v.PublicKeySHA256, CASHA256: sha256.Sum256(a.CertificatePEM()), NotBefore: v.Certificate.NotBefore, NotAfter: v.Certificate.NotAfter, CertificatePEM: v.CertificatePEM}
	}
	return makeReceipt(key), makeReceipt, key
}

func TestMemoryReceiptFirstKeyAndLeafAreImmutable(t *testing.T) {
	first, makeReceipt, key := receiptFixture(t)
	second := makeReceipt(key)
	m, _ := NewMemory(1)
	if bytes.Equal(first.CertificatePEM, second.CertificatePEM) {
		t.Fatal("fixture leaves should differ")
	}
	var wg sync.WaitGroup
	results := make(chan enrollment.Receipt, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := first
			if i%2 == 1 {
				v = second
			}
			r, e := m.GetOrCreate(context.Background(), v)
			if e != nil {
				t.Error(e)
			}
			results <- r
		}(i)
	}
	wg.Wait()
	close(results)
	stored, err := m.Load(context.Background(), first.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	for r := range results {
		if !bytes.Equal(r.CertificatePEM, stored.CertificatePEM) {
			t.Fatal("concurrent create replaced first leaf")
		}
	}
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, e := m.GetOrCreate(context.Background(), makeReceipt(otherKey)); e != enrollment.ErrRejected {
		t.Fatal("different key replaced first")
	}
	second.AssignmentID = "assignment-b"
	if _, e := m.GetOrCreate(context.Background(), second); e != enrollment.ErrRejected {
		t.Fatal("slot/epoch pin or capacity bypassed")
	}
	stored.CertificatePEM[0] ^= 1
	again, _ := m.Load(context.Background(), first.AssignmentID)
	if bytes.Equal(again.CertificatePEM, stored.CertificatePEM) {
		t.Fatal("load exposed stored bytes")
	}
}

func TestMemoryReceiptCancellationAndCapacity(t *testing.T) {
	for _, n := range []int{0, -1, 10001} {
		if _, e := NewMemory(n); e != enrollment.ErrRejected {
			t.Fatal("unbounded capacity accepted")
		}
	}
	first, _, _ := receiptFixture(t)
	m, _ := NewMemory(2)
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	done := make(chan error, 1)
	go func() { _, e := m.GetOrCreate(ctx, first); done <- e }()
	cancel()
	m.mu.Unlock()
	if <-done != enrollment.ErrRejected || len(m.receipts) != 0 {
		t.Fatal("cancelled create wrote receipt")
	}
	if _, e := m.Load(context.Background(), first.AssignmentID); e != enrollment.ErrReceiptNotFound {
		t.Fatal("missing receipt not classified")
	}
	bad := first
	bad.CertificatePEM = append(bytes.Clone(first.CertificatePEM), []byte("private-key-shaped-trailer")...)
	if _, e := m.GetOrCreate(context.Background(), bad); e != enrollment.ErrRejected {
		t.Fatal("non-leaf payload persisted")
	}
}

var receiptColumns = []string{"assignment_id", "account_hash", "slot_id", "node_id", "execution_epoch", "runtime_generation", "public_key_sha256", "ca_sha256", "not_before", "expires_at", "certificate_pem"}

func receiptRow(r enrollment.Receipt) *sqlmock.Rows {
	return sqlmock.NewRows(receiptColumns).AddRow(r.AssignmentID, r.Binding.AccountHash, r.Binding.SlotID, r.Binding.NodeID, r.Binding.Epoch, r.Binding.Generation, r.PublicKeySHA256[:], r.CASHA256[:], r.NotBefore, r.NotAfter, r.CertificatePEM)
}

func TestSQLReceiptAtomicFirstWriterContract(t *testing.T) {
	for _, mode := range []string{"first", "same-key-new-leaf", "different-key", "different-binding", "commit-failure", "cancel-before-commit"} {
		t.Run(mode, func(t *testing.T) {
			first, makeReceipt, key := receiptFixture(t)
			candidate := first
			stored := first
			switch mode {
			case "same-key-new-leaf":
				candidate = makeReceipt(key)
			case "different-key":
				other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				candidate = makeReceipt(other)
			case "different-binding":
				stored.AssignmentID = "other"
			}
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repo, _ := NewSQL(db)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO runtime_certificates").WithArgs(candidate.AssignmentID, candidate.Binding.AccountHash, candidate.Binding.SlotID, candidate.Binding.NodeID, candidate.Binding.Epoch, candidate.Binding.Generation, candidate.PublicKeySHA256[:], candidate.CASHA256[:], candidate.NotBefore.UTC(), candidate.NotAfter.UTC(), candidate.CertificatePEM).WillReturnResult(sqlmock.NewResult(0, 1))
			query := mock.ExpectQuery("SELECT assignment_id.*FROM runtime_certificates WHERE BINARY assignment_id = BINARY \\? FOR UPDATE").WithArgs(candidate.AssignmentID)
			if mode == "cancel-before-commit" {
				query.WillReturnError(context.Canceled)
				mock.ExpectRollback()
			} else {
				query.WillReturnRows(receiptRow(stored))
				switch mode {
				case "different-key", "different-binding":
					mock.ExpectRollback()
				case "commit-failure":
					mock.ExpectCommit().WillReturnError(errors.New("synthetic SQL failure"))
				default:
					mock.ExpectCommit()
				}
			}
			got, e := repo.GetOrCreate(ctx, candidate)
			if mode == "first" || mode == "same-key-new-leaf" {
				if e != nil || !bytes.Equal(got.CertificatePEM, first.CertificatePEM) {
					t.Fatal("did not return committed first leaf", e)
				}
			} else if e != enrollment.ErrRejected || len(got.CertificatePEM) != 0 {
				t.Fatal("failed write returned receipt or nonfixed error")
			}
			if e := mock.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestSQLReceiptLoadBoundedShapeAndFixedErrors(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "database-error", "corrupt", "wrong-id"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := receiptFixture(t)
			id := r.AssignmentID
			db, mock, e := sqlmock.New()
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			repo, _ := NewSQL(db)
			q := mock.ExpectQuery("SELECT assignment_id").WithArgs(id)
			switch mode {
			case "missing":
				q.WillReturnError(sql.ErrNoRows)
			case "database-error":
				q.WillReturnError(errors.New("synthetic sensitive DB diagnostics"))
			case "corrupt":
				r.CertificatePEM = []byte("not a leaf")
				q.WillReturnRows(receiptRow(r))
			case "wrong-id":
				r.AssignmentID = "other"
				q.WillReturnRows(receiptRow(r))
			default:
				q.WillReturnRows(receiptRow(r))
			}
			got, e := repo.Load(context.Background(), id)
			switch mode {
			case "valid":
				if e != nil || !bytes.Equal(got.CertificatePEM, r.CertificatePEM) {
					t.Fatal(e)
				}
			case "missing":
				if e != enrollment.ErrReceiptNotFound {
					t.Fatal(e)
				}
			default:
				if e != enrollment.ErrRejected || len(got.CertificatePEM) != 0 {
					t.Fatal("nonfixed or nonempty denial")
				}
			}
			if e := mock.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
