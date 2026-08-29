package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

// MemoryRepository is a concurrency-safe implementation used by local
// integration tests and development binaries. Production uses Repository.
type MemoryRepository struct {
	mu                       sync.RWMutex
	enrollments              map[[32]byte]memoryEnrollment
	nodes                    map[string]Node
	certificates             map[string]Certificate
	commandResults           map[string]CommandResult
	slots                    map[string]Slot
	assignments              map[string][]Assignment
	jobs                     *MemoryJobRepository
	executionLeases          map[string]ExecutionLease
	credentialVaults         map[string]memoryCredentialVault
	credentialVersions       map[string]credential.VersionRecord
	credentialVersionIDs     map[string][]string
	credentialLeases         map[[32]byte]credential.LeaseRecord
	credentialSecurityEvents []credential.SecurityEvent
}

type memoryEnrollment struct {
	record     Enrollment
	consumedAt *time.Time
	consumedBy string
}

type memoryCredentialVault struct {
	ActiveVersionID string
	AuthType        string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		enrollments:          make(map[[32]byte]memoryEnrollment),
		nodes:                make(map[string]Node),
		certificates:         make(map[string]Certificate),
		commandResults:       make(map[string]CommandResult),
		slots:                make(map[string]Slot),
		assignments:          make(map[string][]Assignment),
		jobs:                 NewMemoryJobRepository(),
		executionLeases:      make(map[string]ExecutionLease),
		credentialVaults:     make(map[string]memoryCredentialVault),
		credentialVersions:   make(map[string]credential.VersionRecord),
		credentialVersionIDs: make(map[string][]string),
		credentialLeases:     make(map[[32]byte]credential.LeaseRecord),
	}
}

func (r *MemoryRepository) CreateEnrollment(_ context.Context, enrollment Enrollment) error {
	if enrollment.ID == "" || enrollment.ExpiresAt.IsZero() || !enrollment.ExpiresAt.After(enrollment.CreatedAt) {
		return errors.New("invalid enrollment record")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.enrollments[enrollment.TokenSHA256]; exists {
		return errors.New("enrollment token digest already exists")
	}
	r.enrollments[enrollment.TokenSHA256] = memoryEnrollment{record: enrollment}
	return nil
}

func (r *MemoryRepository) CommitEnrollment(_ context.Context, tokenSHA256 [32]byte, node Node, certificate Certificate, consumedAt time.Time) error {
	if err := validateEnrollmentCommit(node, certificate, consumedAt); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	enrollment, exists := r.enrollments[tokenSHA256]
	if !exists || enrollment.consumedAt != nil || !enrollment.record.ExpiresAt.After(consumedAt) ||
		(enrollment.record.ExpectedNodeID != "" && enrollment.record.ExpectedNodeID != node.ID) {
		return ErrEnrollmentRejected
	}
	if _, exists := r.nodes[node.ID]; exists {
		return ErrEnrollmentRejected
	}
	if _, exists := r.certificates[certificate.SerialNumber]; exists {
		return ErrEnrollmentRejected
	}
	consumed := consumedAt.UTC()
	enrollment.consumedAt = &consumed
	enrollment.consumedBy = node.ID
	r.enrollments[tokenSHA256] = enrollment
	r.nodes[node.ID] = cloneNode(node)
	r.certificates[certificate.SerialNumber] = certificate
	return nil
}

func (r *MemoryRepository) RotateCertificate(_ context.Context, nodeID, previousSerial string, replacement Certificate, rotatedAt time.Time) error {
	if nodeID == "" || previousSerial == "" || replacement.NodeID != nodeID || replacement.Status != "active" {
		return errors.New("invalid certificate rotation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, exists := r.certificates[previousSerial]
	if !exists || previous.NodeID != nodeID || previous.Status != "active" || !previous.ExpiresAt.After(rotatedAt) {
		return ErrCertificateNotActive
	}
	if _, exists := r.certificates[replacement.SerialNumber]; exists {
		return errors.New("replacement certificate already exists")
	}
	previous.Status = "rotated"
	r.certificates[previousSerial] = previous
	r.certificates[replacement.SerialNumber] = replacement
	return nil
}

func (r *MemoryRepository) ValidateCertificate(_ context.Context, nodeID, serialNumber string, checkedAt time.Time) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	certificate, exists := r.certificates[serialNumber]
	if !exists || certificate.NodeID != nodeID || certificate.Status != "active" || !certificate.ExpiresAt.After(checkedAt) {
		return ErrCertificateNotActive
	}
	return nil
}

func (r *MemoryRepository) AcceptHello(_ context.Context, hello Hello) error {
	if hello.NodeID == "" || hello.SessionID == "" || hello.ProtocolMajor == 0 || hello.ReceivedAt.IsZero() {
		return errors.New("invalid node hello")
	}
	if err := hello.Capacity.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	node, exists := r.nodes[hello.NodeID]
	if !exists || (node.Status != "active" && node.Status != "connected" && node.Status != "disconnected") {
		return ErrNodeNotFound
	}
	node.Labels = cloneLabels(hello.Labels)
	node.Capabilities = append([]string(nil), hello.Capabilities...)
	node.ProtocolMajor = hello.ProtocolMajor
	node.ProtocolMinor = hello.ProtocolMinor
	node.ControlSessionID = hello.SessionID
	node.Capacity = hello.Capacity
	node.Status = "connected"
	node.LastSeenAt = timePointer(hello.ReceivedAt)
	node.DisconnectedAt = nil
	node.UpdatedAt = hello.ReceivedAt.UTC()
	r.nodes[hello.NodeID] = node
	return nil
}

func (r *MemoryRepository) RecordHeartbeat(_ context.Context, heartbeat Heartbeat) error {
	if heartbeat.NodeID == "" || heartbeat.SessionID == "" || heartbeat.ReceivedAt.IsZero() {
		return errors.New("invalid node heartbeat")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	node, exists := r.nodes[heartbeat.NodeID]
	if !exists || node.Status != "connected" || node.ControlSessionID != heartbeat.SessionID || heartbeat.AllocatedSlots > node.Capacity.MaxSlots ||
		heartbeat.AllocatedCPUMillis > node.Capacity.AllocatableCPUMillis || heartbeat.AllocatedMemoryBytes > node.Capacity.AllocatableMemoryBytes ||
		heartbeat.ActiveCLI > node.Capacity.MaxActiveCLI || heartbeat.ActiveAPI > node.Capacity.MaxActiveAPI ||
		heartbeat.ActiveTotal > node.Capacity.MaxActiveTotal {
		return ErrNodeNotFound
	}
	node.AllocatedSlots = heartbeat.AllocatedSlots
	node.AllocatedCPUMillis = heartbeat.AllocatedCPUMillis
	node.AllocatedMemoryBytes = heartbeat.AllocatedMemoryBytes
	node.ActiveCLI = heartbeat.ActiveCLI
	node.ActiveAPI = heartbeat.ActiveAPI
	node.ActiveTotal = heartbeat.ActiveTotal
	node.LastSeenAt = timePointer(heartbeat.ReceivedAt)
	node.UpdatedAt = heartbeat.ReceivedAt.UTC()
	r.nodes[heartbeat.NodeID] = node
	return nil
}

func (r *MemoryRepository) RecordCommandResult(_ context.Context, result CommandResult) error {
	if result.CommandID == "" || result.NodeID == "" || result.ReceivedAt.IsZero() {
		return errors.New("invalid command result")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.commandResults[result.CommandID]; !exists {
		result.SlotObservationJSON = append([]byte(nil), result.SlotObservationJSON...)
		r.commandResults[result.CommandID] = result
	}
	return nil
}

func (r *MemoryRepository) MarkDisconnected(_ context.Context, nodeID, sessionID string, disconnectedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, exists := r.nodes[nodeID]
	if !exists {
		return ErrNodeNotFound
	}
	if node.Status == "connected" && node.ControlSessionID == sessionID {
		node.Status = "disconnected"
		node.ControlSessionID = ""
		node.DisconnectedAt = timePointer(disconnectedAt)
		node.UpdatedAt = disconnectedAt.UTC()
		r.nodes[nodeID] = node
	}
	return nil
}

func (r *MemoryRepository) GetNode(_ context.Context, nodeID string) (Node, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, exists := r.nodes[nodeID]
	if !exists {
		return Node{}, ErrNodeNotFound
	}
	return cloneNode(node), nil
}

func (r *MemoryRepository) GetCommandResult(commandID string) (CommandResult, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result, exists := r.commandResults[commandID]
	result.SlotObservationJSON = append([]byte(nil), result.SlotObservationJSON...)
	return result, exists
}

func cloneNode(node Node) Node {
	node.Labels = cloneLabels(node.Labels)
	node.Capabilities = append([]string(nil), node.Capabilities...)
	if node.LastSeenAt != nil {
		node.LastSeenAt = timePointer(*node.LastSeenAt)
	}
	if node.DisconnectedAt != nil {
		node.DisconnectedAt = timePointer(*node.DisconnectedAt)
	}
	return node
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

var _ NodeRepository = (*MemoryRepository)(nil)
