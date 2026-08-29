package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrEnrollmentRejected   = errors.New("node enrollment is invalid, expired or already consumed")
	ErrNodeNotFound         = errors.New("runtime node not found or inactive")
	ErrCertificateNotActive = errors.New("node certificate is not active")
)

type Capacity struct {
	MaxSlots               uint32
	MaxActiveCLI           uint32
	MaxActiveAPI           uint32
	MaxActiveTotal         uint32
	AllocatableCPUMillis   uint64
	AllocatableMemoryBytes uint64
}

func (c Capacity) Validate() error {
	if c.MaxSlots == 0 || c.MaxActiveCLI == 0 || c.MaxActiveAPI == 0 || c.MaxActiveTotal == 0 ||
		c.AllocatableCPUMillis == 0 || c.AllocatableMemoryBytes == 0 {
		return errors.New("node capacity values must be positive")
	}
	if c.MaxActiveCLI > c.MaxActiveTotal || c.MaxActiveAPI > c.MaxActiveTotal || c.MaxActiveTotal > c.MaxSlots {
		return errors.New("node capacity values are inconsistent")
	}
	if c.AllocatableCPUMillis > 10_000_000 || c.AllocatableMemoryBytes > 1<<60 {
		return errors.New("node resource capacity exceeds supported bounds")
	}
	return nil
}

type Enrollment struct {
	ID             string
	TokenSHA256    [32]byte
	ExpectedNodeID string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

type Node struct {
	ID                   string
	Status               string
	ControlSessionID     string
	Labels               map[string]string
	Capabilities         []string
	ProtocolMajor        uint32
	ProtocolMinor        uint32
	Capacity             Capacity
	ReservedSlots        uint32
	ReservedCPUMillis    uint64
	ReservedMemoryBytes  uint64
	AllocatedSlots       uint32
	AllocatedCPUMillis   uint64
	AllocatedMemoryBytes uint64
	ActiveCLI            uint32
	ActiveAPI            uint32
	ActiveTotal          uint32
	LastSeenAt           *time.Time
	DisconnectedAt       *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Certificate struct {
	SerialNumber      string
	NodeID            string
	CertificateSHA256 [32]byte
	PublicKeySHA256   [32]byte
	Status            string
	NotBefore         time.Time
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

type Hello struct {
	NodeID        string
	SessionID     string
	Labels        map[string]string
	Capabilities  []string
	ProtocolMajor uint32
	ProtocolMinor uint32
	Capacity      Capacity
	ReceivedAt    time.Time
}

type Heartbeat struct {
	NodeID               string
	SessionID            string
	ActiveCLI            uint32
	ActiveAPI            uint32
	ActiveTotal          uint32
	AllocatedSlots       uint32
	AllocatedCPUMillis   uint64
	AllocatedMemoryBytes uint64
	ReceivedAt           time.Time
}

type CommandResult struct {
	CommandID           string
	NodeID              string
	Succeeded           bool
	ErrorCode           string
	ErrorMessage        string
	SlotObservationJSON []byte
	Observation         *AssignmentObservation
	RetryAt             time.Time
	ReceivedAt          time.Time
}

type NodeRepository interface {
	CreateEnrollment(ctx context.Context, enrollment Enrollment) error
	CommitEnrollment(ctx context.Context, tokenSHA256 [32]byte, node Node, certificate Certificate, consumedAt time.Time) error
	RotateCertificate(ctx context.Context, nodeID, previousSerial string, replacement Certificate, rotatedAt time.Time) error
	ValidateCertificate(ctx context.Context, nodeID, serialNumber string, checkedAt time.Time) error
	AcceptHello(ctx context.Context, hello Hello) error
	RecordHeartbeat(ctx context.Context, heartbeat Heartbeat) error
	RecordCommandResult(ctx context.Context, result CommandResult) error
	ApplyCommandResult(ctx context.Context, result CommandResult) error
	MarkDisconnected(ctx context.Context, nodeID, sessionID string, disconnectedAt time.Time) error
	GetNode(ctx context.Context, nodeID string) (Node, error)
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("worker_runtime database is required")
	}
	return &Repository{db: db}, nil
}

func (r *Repository) CreateEnrollment(ctx context.Context, enrollment Enrollment) error {
	if enrollment.ID == "" || enrollment.ExpiresAt.IsZero() || !enrollment.ExpiresAt.After(enrollment.CreatedAt) {
		return errors.New("invalid enrollment record")
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO node_enrollments (
  enrollment_id, token_sha256, expected_node_id, expires_at, created_at
) VALUES (?, ?, NULLIF(?, ''), ?, ?)`,
		enrollment.ID, enrollment.TokenSHA256[:], enrollment.ExpectedNodeID,
		enrollment.ExpiresAt.UTC(), enrollment.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("create node enrollment: %w", err)
	}
	return nil
}

func (r *Repository) CommitEnrollment(ctx context.Context, tokenSHA256 [32]byte, node Node, certificate Certificate, consumedAt time.Time) error {
	if err := validateEnrollmentCommit(node, certificate, consumedAt); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin enrollment transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE node_enrollments
SET consumed_at = ?, consumed_by_node_id = ?
WHERE token_sha256 = ?
  AND consumed_at IS NULL
  AND expires_at > ?
  AND (expected_node_id IS NULL OR expected_node_id = ?)`,
		consumedAt.UTC(), node.ID, tokenSHA256[:], consumedAt.UTC(), node.ID,
	)
	if err != nil {
		return fmt.Errorf("consume node enrollment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read enrollment update result: %w", err)
	}
	if affected != 1 {
		return ErrEnrollmentRejected
	}
	labels, capabilities, err := marshalNodeMetadata(node.Labels, node.Capabilities)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO nodes (
  node_id, status, labels_json, capabilities_json, protocol_major, protocol_minor,
  max_slots, max_active_cli, max_active_api, max_active_total,
  allocatable_cpu_millis, allocatable_memory_bytes, created_at, updated_at
) VALUES (?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		node.ID, labels, capabilities, node.ProtocolMajor, node.ProtocolMinor,
		node.Capacity.MaxSlots, node.Capacity.MaxActiveCLI, node.Capacity.MaxActiveAPI, node.Capacity.MaxActiveTotal,
		node.Capacity.AllocatableCPUMillis, node.Capacity.AllocatableMemoryBytes,
		node.CreatedAt.UTC(), node.UpdatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("insert enrolled node: %w", err)
	}
	if err := insertCertificate(ctx, tx, certificate); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node enrollment: %w", err)
	}
	return nil
}

func (r *Repository) RotateCertificate(ctx context.Context, nodeID, previousSerial string, replacement Certificate, rotatedAt time.Time) error {
	if nodeID == "" || previousSerial == "" || replacement.NodeID != nodeID || replacement.Status != "active" {
		return errors.New("invalid certificate rotation")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin certificate rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertCertificate(ctx, tx, replacement); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE node_certificates
SET status = 'rotated', revoked_at = ?, replaced_by_serial = ?
WHERE serial_number = ? AND node_id = ? AND status = 'active' AND expires_at > ?`,
		rotatedAt.UTC(), replacement.SerialNumber, previousSerial, nodeID, rotatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("retire previous node certificate: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read certificate rotation result: %w", err)
	}
	if affected != 1 {
		return ErrCertificateNotActive
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit certificate rotation: %w", err)
	}
	return nil
}

func (r *Repository) ValidateCertificate(ctx context.Context, nodeID, serialNumber string, checkedAt time.Time) error {
	if nodeID == "" || serialNumber == "" || checkedAt.IsZero() {
		return ErrCertificateNotActive
	}
	var active int
	err := r.db.QueryRowContext(ctx, `
SELECT 1
FROM node_certificates
WHERE node_id = ? AND serial_number = ? AND status = 'active' AND expires_at > ?`,
		nodeID, serialNumber, checkedAt.UTC(),
	).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCertificateNotActive
	}
	if err != nil {
		return fmt.Errorf("validate node certificate: %w", err)
	}
	return nil
}

func (r *Repository) AcceptHello(ctx context.Context, hello Hello) error {
	if hello.NodeID == "" || hello.SessionID == "" || hello.ProtocolMajor == 0 || hello.ReceivedAt.IsZero() {
		return errors.New("invalid node hello")
	}
	if err := hello.Capacity.Validate(); err != nil {
		return err
	}
	labels, capabilities, err := marshalNodeMetadata(hello.Labels, hello.Capabilities)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE nodes SET
  labels_json = ?, capabilities_json = ?, protocol_major = ?, protocol_minor = ?,
  max_slots = ?, max_active_cli = ?, max_active_api = ?, max_active_total = ?,
  allocatable_cpu_millis = ?, allocatable_memory_bytes = ?,
  status = 'connected', control_session_id = ?, disconnected_at = NULL, last_seen_at = ?, updated_at = ?
WHERE node_id = ? AND status IN ('active', 'connected', 'disconnected')`,
		labels, capabilities, hello.ProtocolMajor, hello.ProtocolMinor,
		hello.Capacity.MaxSlots, hello.Capacity.MaxActiveCLI, hello.Capacity.MaxActiveAPI, hello.Capacity.MaxActiveTotal,
		hello.Capacity.AllocatableCPUMillis, hello.Capacity.AllocatableMemoryBytes,
		hello.SessionID, hello.ReceivedAt.UTC(), hello.ReceivedAt.UTC(), hello.NodeID,
	)
	if err != nil {
		return fmt.Errorf("accept node hello: %w", err)
	}
	return requireOneNode(result)
}

func (r *Repository) RecordHeartbeat(ctx context.Context, heartbeat Heartbeat) error {
	if heartbeat.NodeID == "" || heartbeat.SessionID == "" || heartbeat.ReceivedAt.IsZero() {
		return errors.New("invalid node heartbeat")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE nodes SET
  allocated_slots = ?, allocated_cpu_millis = ?, allocated_memory_bytes = ?,
  active_cli = ?, active_api = ?, active_total = ?,
  last_seen_at = ?, updated_at = ?
WHERE node_id = ? AND control_session_id = ? AND status = 'connected'
	AND ? <= max_slots AND ? <= allocatable_cpu_millis AND ? <= allocatable_memory_bytes
  AND ? <= max_active_cli AND ? <= max_active_api AND ? <= max_active_total`,
		heartbeat.AllocatedSlots, heartbeat.AllocatedCPUMillis, heartbeat.AllocatedMemoryBytes,
		heartbeat.ActiveCLI, heartbeat.ActiveAPI, heartbeat.ActiveTotal,
		heartbeat.ReceivedAt.UTC(), heartbeat.ReceivedAt.UTC(), heartbeat.NodeID, heartbeat.SessionID,
		heartbeat.AllocatedSlots, heartbeat.AllocatedCPUMillis, heartbeat.AllocatedMemoryBytes,
		heartbeat.ActiveCLI, heartbeat.ActiveAPI, heartbeat.ActiveTotal,
	)
	if err != nil {
		return fmt.Errorf("record node heartbeat: %w", err)
	}
	return requireOneNode(result)
}

func (r *Repository) RecordCommandResult(ctx context.Context, result CommandResult) error {
	if result.CommandID == "" || result.NodeID == "" || result.ReceivedAt.IsZero() {
		return errors.New("invalid command result")
	}
	var observation any
	if len(result.SlotObservationJSON) > 0 {
		if !json.Valid(result.SlotObservationJSON) {
			return errors.New("slot observation must be valid JSON")
		}
		observation = result.SlotObservationJSON
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO node_command_results (
  command_id, node_id, succeeded, error_code, error_message, slot_observation_json, received_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE command_id = command_id`,
		result.CommandID, result.NodeID, result.Succeeded, result.ErrorCode, result.ErrorMessage,
		observation, result.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("record node command result: %w", err)
	}
	return nil
}

func (r *Repository) MarkDisconnected(ctx context.Context, nodeID, sessionID string, disconnectedAt time.Time) error {
	if nodeID == "" || sessionID == "" || disconnectedAt.IsZero() {
		return errors.New("invalid node disconnect")
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE nodes SET status = 'disconnected', control_session_id = NULL, disconnected_at = ?, updated_at = ?
WHERE node_id = ? AND control_session_id = ? AND status = 'connected'`,
		disconnectedAt.UTC(), disconnectedAt.UTC(), nodeID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("mark node disconnected: %w", err)
	}
	return nil
}

func (r *Repository) GetNode(ctx context.Context, nodeID string) (Node, error) {
	var node Node
	var labels, capabilities []byte
	var lastSeen, disconnected sql.NullTime
	err := r.db.QueryRowContext(ctx, `
SELECT node_id, status, COALESCE(control_session_id, ''), labels_json, capabilities_json, protocol_major, protocol_minor,
       max_slots, max_active_cli, max_active_api, max_active_total, allocatable_cpu_millis, allocatable_memory_bytes,
       reserved_slots, reserved_cpu_millis, reserved_memory_bytes,
       allocated_slots, allocated_cpu_millis, allocated_memory_bytes, active_cli, active_api, active_total,
       last_seen_at, disconnected_at, created_at, updated_at
FROM nodes WHERE node_id = ?`, nodeID).Scan(
		&node.ID, &node.Status, &node.ControlSessionID, &labels, &capabilities, &node.ProtocolMajor, &node.ProtocolMinor,
		&node.Capacity.MaxSlots, &node.Capacity.MaxActiveCLI, &node.Capacity.MaxActiveAPI, &node.Capacity.MaxActiveTotal,
		&node.Capacity.AllocatableCPUMillis, &node.Capacity.AllocatableMemoryBytes,
		&node.ReservedSlots, &node.ReservedCPUMillis, &node.ReservedMemoryBytes,
		&node.AllocatedSlots, &node.AllocatedCPUMillis, &node.AllocatedMemoryBytes, &node.ActiveCLI, &node.ActiveAPI, &node.ActiveTotal,
		&lastSeen, &disconnected, &node.CreatedAt, &node.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNodeNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("get runtime node: %w", err)
	}
	if err := json.Unmarshal(labels, &node.Labels); err != nil {
		return Node{}, fmt.Errorf("decode node labels: %w", err)
	}
	if err := json.Unmarshal(capabilities, &node.Capabilities); err != nil {
		return Node{}, fmt.Errorf("decode node capabilities: %w", err)
	}
	if lastSeen.Valid {
		value := lastSeen.Time.UTC()
		node.LastSeenAt = &value
	}
	if disconnected.Valid {
		value := disconnected.Time.UTC()
		node.DisconnectedAt = &value
	}
	return node, nil
}

func validateEnrollmentCommit(node Node, certificate Certificate, consumedAt time.Time) error {
	if node.ID == "" || node.Status != "active" || node.ProtocolMajor == 0 || consumedAt.IsZero() {
		return errors.New("invalid enrolled node")
	}
	if certificate.SerialNumber == "" || certificate.NodeID != node.ID || certificate.Status != "active" ||
		certificate.NotBefore.IsZero() || !certificate.ExpiresAt.After(certificate.NotBefore) {
		return errors.New("invalid enrolled node certificate")
	}
	return nil
}

func marshalNodeMetadata(labels map[string]string, capabilities []string) ([]byte, []byte, error) {
	if labels == nil {
		labels = map[string]string{}
	}
	if capabilities == nil {
		capabilities = []string{}
	}
	encodedLabels, err := json.Marshal(labels)
	if err != nil {
		return nil, nil, fmt.Errorf("encode node labels: %w", err)
	}
	encodedCapabilities, err := json.Marshal(capabilities)
	if err != nil {
		return nil, nil, fmt.Errorf("encode node capabilities: %w", err)
	}
	return encodedLabels, encodedCapabilities, nil
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertCertificate(ctx context.Context, executor sqlExecutor, certificate Certificate) error {
	_, err := executor.ExecContext(ctx, `
INSERT INTO node_certificates (
  serial_number, node_id, certificate_sha256, public_key_sha256, status,
  not_before, expires_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		certificate.SerialNumber, certificate.NodeID, certificate.CertificateSHA256[:], certificate.PublicKeySHA256[:],
		certificate.Status, certificate.NotBefore.UTC(), certificate.ExpiresAt.UTC(), certificate.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert node certificate: %w", err)
	}
	return nil
}

func requireOneNode(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func HashToken(rawToken string) [32]byte {
	return sha256.Sum256([]byte(rawToken))
}

var _ NodeRepository = (*Repository)(nil)
