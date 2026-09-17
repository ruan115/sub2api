package storage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	enrollment "github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
)

type SQL struct{ db *sql.DB }

func NewSQL(db *sql.DB) (*SQL, error) {
	if db == nil {
		return nil, enrollment.ErrRejected
	}
	return &SQL{db: db}, nil
}

const readReceipt = `SELECT assignment_id, account_hash, slot_id, node_id, execution_epoch, runtime_generation,
 public_key_sha256, ca_sha256, not_before, expires_at, certificate_pem FROM runtime_certificates WHERE BINARY assignment_id = BINARY ?`

type row interface{ Scan(...any) error }

func scanReceipt(row row) (enrollment.Receipt, error) {
	var r enrollment.Receipt
	var hash, caHash []byte
	err := row.Scan(&r.AssignmentID, &r.Binding.AccountHash, &r.Binding.SlotID, &r.Binding.NodeID, &r.Binding.Epoch, &r.Binding.Generation, &hash, &caHash, &r.NotBefore, &r.NotAfter, &r.CertificatePEM)
	if err != nil {
		return enrollment.Receipt{}, err
	}
	if len(hash) != 32 || len(caHash) != 32 {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	copy(r.PublicKeySHA256[:], hash)
	copy(r.CASHA256[:], caHash)
	if enrollment.ValidateReceipt(r) != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	return r, nil
}

func (s *SQL) Load(ctx context.Context, id string) (enrollment.Receipt, error) {
	if s == nil || s.db == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(id) != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	r, err := scanReceipt(s.db.QueryRowContext(ctx, readReceipt, id))
	if ctx.Err() != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	if errors.Is(err, sql.ErrNoRows) {
		return enrollment.Receipt{}, enrollment.ErrReceiptNotFound
	}
	if err != nil || r.AssignmentID != id {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	return enrollment.CloneReceipt(r), nil
}

// The unique assignment and slot/epoch indexes serialize competing writers.
// ON DUPLICATE performs a no-op; only a committed, identically bound first row
// is returned. Another key or assignment never overwrites that first row.
func (s *SQL) GetOrCreate(ctx context.Context, candidate enrollment.Receipt) (enrollment.Receipt, error) {
	if s == nil || s.db == nil || ctx == nil || ctx.Err() != nil || enrollment.ValidateReceipt(candidate) != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_certificates
 (assignment_id, account_hash, slot_id, node_id, execution_epoch, runtime_generation, public_key_sha256, ca_sha256, not_before, expires_at, certificate_pem)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE assignment_id = assignment_id`,
		candidate.AssignmentID, candidate.Binding.AccountHash, candidate.Binding.SlotID, candidate.Binding.NodeID, candidate.Binding.Epoch, candidate.Binding.Generation, candidate.PublicKeySHA256[:], candidate.CASHA256[:], candidate.NotBefore.UTC(), candidate.NotAfter.UTC(), candidate.CertificatePEM)
	if err != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	r, err := scanReceipt(tx.QueryRowContext(ctx, readReceipt+` FOR UPDATE`, candidate.AssignmentID))
	if err != nil || !enrollment.SameReceiptIdentity(r, candidate) || ctx.Err() != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	if tx.Commit() != nil || ctx.Err() != nil {
		return enrollment.Receipt{}, enrollment.ErrRejected
	}
	return enrollment.CloneReceipt(r), nil
}

var _ enrollment.ReceiptStore = (*SQL)(nil)
