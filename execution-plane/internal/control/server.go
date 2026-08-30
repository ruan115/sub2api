package control

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	CurrentProtocolMajor uint32 = 1
	CurrentProtocolMinor uint32 = 1

	secureActivationCapability = "secure_activation"
	maxCredentialBundleBytes   = 2 << 20
)

var (
	nodeIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	metadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	capabilityPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	sensitiveWords     = []string{"authorization", "credential", "password", "cookie", "secret", "session_key", "access_token", "refresh_token", "bearer", "sk-"}
)

type Config struct {
	EnrollmentTTL     time.Duration
	CertificateTTL    time.Duration
	RotateBefore      time.Duration
	HeartbeatTimeout  time.Duration
	CommandRetryDelay time.Duration
	OutboundQueue     int
	CredentialSink    worker.SealedCredentialSink
	CommandObserver   CommandObserver
	Now               func() time.Time
}

// CommandObserver is the durable, idempotent handoff from NodeControl into the
// provisioning workflow. Implementations must tolerate the same command result
// more than once after a stream reconnect or repository retry.
type CommandObserver interface {
	ObserveCommandResult(ctx context.Context, nodeID string, result *executionv1.CommandResult) error
}

func DefaultConfig() Config {
	return Config{
		EnrollmentTTL: 15 * time.Minute, CertificateTTL: 24 * time.Hour,
		RotateBefore: 6 * time.Hour, HeartbeatTimeout: 45 * time.Second,
		CommandRetryDelay: 5 * time.Second, OutboundQueue: 64, Now: time.Now,
	}
}

type EnrollmentToken struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

type Server struct {
	executionv1.UnimplementedNodeControlServiceServer
	repository store.NodeRepository
	authority  *pki.Authority
	config     Config

	mu       sync.RWMutex
	sessions map[string]*nodeSession
}

type nodeSession struct {
	id                 string
	capacity           store.Capacity
	protocolMinor      uint32
	capabilities       map[string]struct{}
	outbound           chan *executionv1.NodeControlServiceControlResponse
	done               chan struct{}
	commandMu          sync.Mutex
	pendingCommands    map[string]pendingCommand
	maxPendingCommands int
}

type pendingCommand struct {
	kind              pendingCommandKind
	slotID            string
	executionEpoch    uint64
	accountBinding    string
	credentialLeaseID string
	proxyLeaseID      string
	imageDigest       string
	deadline          time.Time
	commitStarted     bool
}

type pendingCommandKind uint8

const (
	pendingSlotCommand pendingCommandKind = iota + 1
	pendingEpochRevocation
	pendingCredentialKey
	pendingSecureActivation
)

func NewServer(repository store.NodeRepository, authority *pki.Authority, config Config) (*Server, error) {
	if repository == nil || authority == nil {
		return nil, errors.New("node repository and certificate authority are required")
	}
	if config.EnrollmentTTL <= 0 || config.CertificateTTL <= 0 || config.RotateBefore <= 0 ||
		config.CertificateTTL != authority.CertificateTTL() || config.RotateBefore > config.CertificateTTL ||
		config.HeartbeatTimeout <= 0 || config.CommandRetryDelay <= 0 || config.OutboundQueue <= 0 {
		return nil, errors.New("invalid node control configuration")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Server{repository: repository, authority: authority, config: config, sessions: make(map[string]*nodeSession)}, nil
}

// CreateEnrollment returns the raw one-time token exactly once. The repository
// receives only its SHA-256 digest.
func (s *Server) CreateEnrollment(ctx context.Context, expectedNodeID string) (EnrollmentToken, error) {
	if expectedNodeID != "" && !nodeIDPattern.MatchString(expectedNodeID) {
		return EnrollmentToken{}, errors.New("expected node id is invalid")
	}
	id, err := randomHex(16)
	if err != nil {
		return EnrollmentToken{}, err
	}
	raw, err := randomToken(32)
	if err != nil {
		return EnrollmentToken{}, err
	}
	now := s.config.Now().UTC()
	expiresAt := now.Add(s.config.EnrollmentTTL)
	if err := s.repository.CreateEnrollment(ctx, store.Enrollment{
		ID: id, TokenSHA256: store.HashToken(raw), ExpectedNodeID: expectedNodeID,
		ExpiresAt: expiresAt, CreatedAt: now,
	}); err != nil {
		return EnrollmentToken{}, err
	}
	return EnrollmentToken{ID: id, Token: raw, ExpiresAt: expiresAt}, nil
}

func (s *Server) EnrollNode(ctx context.Context, request *executionv1.EnrollNodeRequest) (*executionv1.EnrollNodeResponse, error) {
	if request == nil || request.GetEnrollmentToken() == "" || !nodeIDPattern.MatchString(request.GetNodeId()) || request.GetPublicKeyPem() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid node enrollment request")
	}
	if err := validateProtocol(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	if err := validateMetadata(request.GetLabels(), request.GetCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	issued, err := s.authority.IssueNode(request.GetNodeId(), []byte(request.GetPublicKeyPem()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "node public key is invalid")
	}
	now := s.config.Now().UTC()
	node := store.Node{
		ID: request.GetNodeId(), Status: "active", Labels: cloneLabels(request.GetLabels()),
		Capabilities:  append([]string(nil), request.GetCapabilities()...),
		ProtocolMajor: request.GetProtocolVersion().GetMajor(), ProtocolMinor: request.GetProtocolVersion().GetMinor(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repository.CommitEnrollment(ctx, store.HashToken(request.GetEnrollmentToken()), node, certificateRecord(request.GetNodeId(), issued, now), now); err != nil {
		if errors.Is(err, store.ErrEnrollmentRejected) {
			return nil, status.Error(codes.PermissionDenied, "node enrollment rejected")
		}
		return nil, status.Error(codes.Internal, "node enrollment failed")
	}
	return &executionv1.EnrollNodeResponse{
		NodeId: request.GetNodeId(), CertificatePem: string(issued.CertificatePEM),
		CaCertificatePem:     string(s.authority.CertificatePEM()),
		CertificateExpiresAt: timestamppb.New(issued.Certificate.NotAfter),
	}, nil
}

func (s *Server) RenewNodeCertificate(ctx context.Context, request *executionv1.RenewNodeCertificateRequest) (*executionv1.RenewNodeCertificateResponse, error) {
	identity, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if request == nil || request.GetNodeId() != identity.nodeID || request.GetPublicKeyPem() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid certificate renewal request")
	}
	now := s.config.Now().UTC()
	if identity.certificate.NotAfter.Sub(now) > s.config.RotateBefore {
		return nil, status.Error(codes.FailedPrecondition, "certificate is not in its rotation window")
	}
	issued, issueErr := s.authority.IssueNode(identity.nodeID, []byte(request.GetPublicKeyPem()))
	if issueErr != nil {
		return nil, status.Error(codes.InvalidArgument, "node public key is invalid")
	}
	if rotateErr := s.repository.RotateCertificate(ctx, identity.nodeID, pki.SerialString(identity.certificate.SerialNumber), certificateRecord(identity.nodeID, issued, now), now); rotateErr != nil {
		if errors.Is(rotateErr, store.ErrCertificateNotActive) {
			return nil, status.Error(codes.Unauthenticated, "node certificate is not active")
		}
		return nil, status.Error(codes.Internal, "certificate renewal failed")
	}
	return &executionv1.RenewNodeCertificateResponse{
		NodeId: identity.nodeID, CertificatePem: string(issued.CertificatePEM),
		CaCertificatePem:     string(s.authority.CertificatePEM()),
		CertificateExpiresAt: timestamppb.New(issued.Certificate.NotAfter),
	}, nil
}

func (s *Server) Control(stream executionv1.NodeControlService_ControlServer) error {
	identity, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	firstInbound := make(chan receiveResult, 1)
	go receiveControl(stream, firstInbound)
	helloTimer := time.NewTimer(s.config.HeartbeatTimeout)
	defer helloTimer.Stop()
	var first *executionv1.NodeControlServiceControlRequest
	select {
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-helloTimer.C:
		return status.Error(codes.DeadlineExceeded, "node hello timeout")
	case result := <-firstInbound:
		if result.err != nil {
			return result.err
		}
		first = result.request
	}
	hello := first.GetHello()
	if hello == nil || hello.GetNodeId() != identity.nodeID {
		return status.Error(codes.InvalidArgument, "first control event must be the authenticated node hello")
	}
	if err := s.validatePeerCertificate(stream.Context(), identity); err != nil {
		return err
	}
	if err := validateProtocol(hello.GetProtocolVersion()); err != nil {
		return err
	}
	if err := validateMetadata(hello.GetLabels(), hello.GetCapabilities()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	capacity, err := capacityFromProto(hello.GetCapacity())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	sessionID, err := randomHex(16)
	if err != nil {
		return status.Error(codes.Internal, "create node session")
	}
	session := &nodeSession{
		id: sessionID, capacity: capacity, protocolMinor: hello.GetProtocolVersion().GetMinor(),
		capabilities:       capabilitySet(hello.GetCapabilities()),
		outbound:           make(chan *executionv1.NodeControlServiceControlResponse, s.config.OutboundQueue),
		done:               make(chan struct{}),
		pendingCommands:    make(map[string]pendingCommand),
		maxPendingCommands: s.config.OutboundQueue * 4,
	}
	if !s.attach(identity.nodeID, session) {
		return status.Error(codes.AlreadyExists, "node already has an active control stream")
	}
	now := s.config.Now().UTC()
	if err := s.repository.AcceptHello(stream.Context(), store.Hello{
		NodeID: identity.nodeID, SessionID: sessionID,
		Labels: cloneLabels(hello.GetLabels()), Capabilities: append([]string(nil), hello.GetCapabilities()...),
		ProtocolMajor: hello.GetProtocolVersion().GetMajor(), ProtocolMinor: hello.GetProtocolVersion().GetMinor(),
		Capacity: capacity, ReceivedAt: now,
	}); err != nil {
		s.detach(identity.nodeID, sessionID)
		return status.Error(codes.FailedPrecondition, "node hello rejected")
	}
	defer func() {
		s.detach(identity.nodeID, sessionID)
		disconnectContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.repository.MarkDisconnected(disconnectContext, identity.nodeID, sessionID, s.config.Now().UTC())
	}()

	inbound := make(chan receiveResult, 1)
	go receiveControl(stream, inbound)
	timer := time.NewTimer(s.config.HeartbeatTimeout)
	defer timer.Stop()
	outbound := (<-chan *executionv1.NodeControlServiceControlResponse)(session.outbound)
	var sendDone <-chan error
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-timer.C:
			return status.Error(codes.DeadlineExceeded, "node heartbeat timeout")
		case response := <-outbound:
			result := make(chan error, 1)
			sendDone = result
			outbound = nil
			go func() { result <- stream.Send(response) }()
		case err := <-sendDone:
			if err != nil {
				return err
			}
			sendDone = nil
			outbound = session.outbound
		case result := <-inbound:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					return nil
				}
				return result.err
			}
			if result.request.GetHello() != nil {
				return status.Error(codes.InvalidArgument, "node hello may only be sent once")
			}
			if err := s.validatePeerCertificate(stream.Context(), identity); err != nil {
				return err
			}
			if heartbeat := result.request.GetHeartbeat(); heartbeat != nil {
				if err := s.recordHeartbeat(stream.Context(), identity.nodeID, sessionID, capacity, heartbeat); err != nil {
					return err
				}
				resetTimer(timer, s.config.HeartbeatTimeout)
			} else if commandResult := result.request.GetCommandResult(); commandResult != nil {
				if err := s.recordCommandResult(stream.Context(), identity.nodeID, session, commandResult); err != nil {
					return err
				}
			} else if credentialCommit := result.request.GetCredentialCommit(); credentialCommit != nil {
				if err := s.recordCredentialCommit(stream.Context(), session, credentialCommit); err != nil {
					return err
				}
			} else {
				return status.Error(codes.InvalidArgument, "control event is required")
			}
			go receiveControl(stream, inbound)
		}
	}
}

func (s *Server) Dispatch(ctx context.Context, nodeID string, response *executionv1.NodeControlServiceControlResponse) error {
	if !nodeIDPattern.MatchString(nodeID) {
		return errors.New("node id is invalid")
	}
	if response == nil {
		return errors.New("control response is required")
	}
	cloned, ok := proto.Clone(response).(*executionv1.NodeControlServiceControlResponse)
	if !ok {
		return errors.New("control response is invalid")
	}
	if err := validateControlResponse(cloned); err != nil {
		return err
	}
	if deadline := controlDeadline(cloned); !deadline.IsZero() && !deadline.After(s.config.Now().UTC()) {
		return errors.New("control command deadline has expired")
	}
	commandID := controlCommandID(cloned)
	s.mu.RLock()
	session := s.sessions[nodeID]
	s.mu.RUnlock()
	if session == nil {
		return store.ErrNodeNotFound
	}
	if cloned.GetCredentialKeyCommand() != nil {
		if s.config.CommandObserver == nil {
			return errors.New("credential-key command observer is not configured")
		}
		if !session.supportsSecureActivation() {
			return errors.New("node does not support secure activation control commands")
		}
	}
	if cloned.GetSecureActivationCommand() != nil {
		if s.config.CredentialSink == nil {
			return errors.New("secure activation credential sink is not configured")
		}
		if s.config.CommandObserver == nil {
			return errors.New("secure activation command observer is not configured")
		}
		if !session.supportsSecureActivation() {
			return errors.New("node does not support secure activation control commands")
		}
	}
	if !session.reserveCommand(commandID, pendingFromResponse(cloned)) {
		return errors.New("control command is duplicate or node command capacity is full")
	}
	queued := false
	defer func() {
		if !queued {
			session.releaseCommand(commandID)
		}
	}()
	select {
	case <-session.done:
		return store.ErrNodeNotFound
	default:
	}
	select {
	case session.outbound <- cloned:
		queued = true
		return nil
	case <-session.done:
		return store.ErrNodeNotFound
	case <-ctx.Done():
		return ctx.Err()
	}
}

type peerIdentity struct {
	nodeID      string
	certificate *x509.Certificate
}

func (s *Server) authenticate(ctx context.Context) (peerIdentity, error) {
	remote, ok := peer.FromContext(ctx)
	if !ok || remote.AuthInfo == nil {
		return peerIdentity{}, status.Error(codes.Unauthenticated, "node client certificate is required")
	}
	tlsInfo, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 || len(tlsInfo.State.VerifiedChains) == 0 {
		return peerIdentity{}, status.Error(codes.Unauthenticated, "node client certificate is required")
	}
	certificate := tlsInfo.State.PeerCertificates[0]
	nodeID, err := pki.NodeIDFromCertificate(certificate)
	if err != nil {
		return peerIdentity{}, status.Error(codes.Unauthenticated, "node identity certificate is invalid")
	}
	identity := peerIdentity{nodeID: nodeID, certificate: certificate}
	if err := s.validatePeerCertificate(ctx, identity); err != nil {
		return peerIdentity{}, err
	}
	return identity, nil
}

func (s *Server) validatePeerCertificate(ctx context.Context, identity peerIdentity) error {
	if err := s.repository.ValidateCertificate(ctx, identity.nodeID, pki.SerialString(identity.certificate.SerialNumber), s.config.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrCertificateNotActive) {
			return status.Error(codes.Unauthenticated, "node certificate is not active")
		}
		return status.Error(codes.Internal, "node certificate validation failed")
	}
	return nil
}

func (s *Server) attach(nodeID string, session *nodeSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[nodeID] != nil {
		return false
	}
	s.sessions[nodeID] = session
	return true
}

func (s *Server) detach(nodeID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.sessions[nodeID]; current != nil && current.id == sessionID {
		delete(s.sessions, nodeID)
		close(current.done)
	}
}

type receiveResult struct {
	request *executionv1.NodeControlServiceControlRequest
	err     error
}

func receiveControl(stream executionv1.NodeControlService_ControlServer, destination chan<- receiveResult) {
	request, err := stream.Recv()
	destination <- receiveResult{request: request, err: err}
}

func (s *Server) recordHeartbeat(ctx context.Context, nodeID, sessionID string, capacity store.Capacity, heartbeat *executionv1.NodeHeartbeat) error {
	if heartbeat.GetNodeId() != nodeID {
		return status.Error(codes.InvalidArgument, "heartbeat node id does not match authenticated node")
	}
	if heartbeat.GetObservedAt() == nil {
		return status.Error(codes.InvalidArgument, "heartbeat observed_at is required")
	}
	if err := heartbeat.GetObservedAt().CheckValid(); err != nil {
		return status.Error(codes.InvalidArgument, "heartbeat observed_at is invalid")
	}
	if heartbeat.GetAllocatedSlots() > capacity.MaxSlots || heartbeat.GetActiveCli() > capacity.MaxActiveCLI ||
		heartbeat.GetActiveApi() > capacity.MaxActiveAPI || heartbeat.GetActiveTotal() > capacity.MaxActiveTotal ||
		heartbeat.GetAllocatedCpuMillis() > capacity.AllocatableCPUMillis ||
		heartbeat.GetAllocatedMemoryBytes() > capacity.AllocatableMemoryBytes ||
		heartbeat.GetActiveCli() > heartbeat.GetActiveTotal() || heartbeat.GetActiveApi() > heartbeat.GetActiveTotal() ||
		uint32(len(heartbeat.GetSlots())) > capacity.MaxSlots {
		return status.Error(codes.InvalidArgument, "heartbeat exceeds declared capacity")
	}
	seenSlots := make(map[string]struct{}, len(heartbeat.GetSlots()))
	for _, slot := range heartbeat.GetSlots() {
		if err := validateSlotObservation(slot); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if _, exists := seenSlots[slot.GetSlotId()]; exists {
			return status.Error(codes.InvalidArgument, "heartbeat contains duplicate slot observations")
		}
		seenSlots[slot.GetSlotId()] = struct{}{}
	}
	if err := s.repository.RecordHeartbeat(ctx, store.Heartbeat{
		NodeID: nodeID, SessionID: sessionID,
		ActiveCLI: heartbeat.GetActiveCli(), ActiveAPI: heartbeat.GetActiveApi(),
		ActiveTotal: heartbeat.GetActiveTotal(), AllocatedSlots: heartbeat.GetAllocatedSlots(),
		AllocatedCPUMillis: heartbeat.GetAllocatedCpuMillis(), AllocatedMemoryBytes: heartbeat.GetAllocatedMemoryBytes(),
		ReceivedAt: s.config.Now().UTC(),
	}); err != nil {
		return status.Error(codes.FailedPrecondition, "node heartbeat rejected")
	}
	return nil
}

func (s *Server) recordCommandResult(ctx context.Context, nodeID string, session *nodeSession, result *executionv1.CommandResult) error {
	if result.GetCommandId() == "" || len(result.GetCommandId()) > 128 || len(result.GetErrorCode()) > 64 || containsSensitiveWord(result.GetErrorCode()) {
		return status.Error(codes.InvalidArgument, "command result identity is invalid")
	}
	pending, issued := session.command(result.GetCommandId())
	if !issued {
		return status.Error(codes.PermissionDenied, "command result was not issued to this node session")
	}
	if result.GetSlot() == nil || result.GetSlot().GetSlotId() != pending.slotID || result.GetSlot().GetExecutionEpoch() != pending.executionEpoch {
		return status.Error(codes.InvalidArgument, "command result does not match its issued slot and epoch")
	}
	if err := validateSlotObservation(result.GetSlot()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if pending.kind == pendingCredentialKey {
		if result.GetSlot().GetImageDigest() != pending.imageDigest {
			return status.Error(codes.InvalidArgument, "credential-key result image does not match its issued command")
		}
		if result.GetSucceeded() {
			transportKey := result.GetCredentialTransportKey()
			if transportKey == nil || credential.ValidateRecipientKey(transportKey.GetKeyId(), transportKey.GetPublicKey()) != nil {
				return status.Error(codes.InvalidArgument, "credential transport key result is invalid")
			}
		} else if result.GetCredentialTransportKey() != nil {
			return status.Error(codes.InvalidArgument, "failed credential-key result must not contain a transport key")
		}
	} else if pending.kind == pendingSecureActivation && result.GetSlot().GetImageDigest() != pending.imageDigest {
		return status.Error(codes.InvalidArgument, "secure activation result image does not match its issued command")
	} else if result.GetCredentialTransportKey() != nil {
		return status.Error(codes.InvalidArgument, "command result contains an unexpected credential transport key")
	}
	if pending.kind == pendingCredentialKey || pending.kind == pendingSecureActivation {
		observed, ok := proto.Clone(result).(*executionv1.CommandResult)
		if !ok || s.config.CommandObserver == nil || s.config.CommandObserver.ObserveCommandResult(ctx, nodeID, observed) != nil {
			return status.Error(codes.Internal, "observe secure onboarding command result failed")
		}
	}
	observation, err := protojson.Marshal(result.GetSlot())
	if err != nil {
		return status.Error(codes.InvalidArgument, "slot observation is invalid")
	}
	now := s.config.Now().UTC()
	errorCode := result.GetErrorCode()
	if !result.GetSucceeded() && errorCode == "" {
		errorCode = "node_command_failed"
	}
	if err := s.repository.ApplyCommandResult(ctx, store.CommandResult{
		CommandID: result.GetCommandId(), NodeID: nodeID, Succeeded: result.GetSucceeded(),
		ErrorCode: errorCode, ErrorMessage: sanitizeError(result.GetErrorMessage()),
		SlotObservationJSON: observation,
		Observation: &store.AssignmentObservation{
			SlotID: result.GetSlot().GetSlotId(), ExecutionEpoch: result.GetSlot().GetExecutionEpoch(),
			ProviderRef: result.GetSlot().GetProviderRef(), ActualState: result.GetSlot().GetActualState(),
			Healthy: result.GetSlot().GetHealthy(), ReasonCode: sanitizeReasonCode(result.GetSlot().GetReason()), ObservedAt: now,
		},
		RetryAt: now.Add(s.config.CommandRetryDelay), ReceivedAt: now,
	}); err != nil {
		return status.Error(codes.Internal, "record command result failed")
	}
	session.releaseCommand(result.GetCommandId())
	return nil
}

func (s *Server) recordCredentialCommit(ctx context.Context, session *nodeSession, commit *executionv1.ControlCredentialCommit) error {
	if s.config.CredentialSink == nil {
		return status.Error(codes.FailedPrecondition, "secure activation credential sink is unavailable")
	}
	pending, err := session.beginCredentialCommit(commit)
	if err != nil {
		return status.Error(codes.InvalidArgument, "credential commit does not match its secure activation command")
	}
	commandID := commit.GetCommandId()
	sealed := append([]byte(nil), commit.GetSealedCredentialBundle()...)
	zeroControlBytes(commit.SealedCredentialBundle)
	commit.SealedCredentialBundle = nil
	go func() {
		defer zeroControlBytes(sealed)
		commitContext := ctx
		cancel := func() {}
		if !pending.deadline.IsZero() {
			commitContext, cancel = context.WithDeadline(ctx, pending.deadline)
		}
		defer cancel()
		versionID, commitErr := s.config.CredentialSink.CommitSealedCredential(commitContext, worker.SealedCredentialCommitRequest{
			AccountBinding: pending.accountBinding, SlotID: pending.slotID, ExecutionEpoch: pending.executionEpoch,
			CredentialLeaseID: pending.credentialLeaseID, ProxyLeaseID: pending.proxyLeaseID,
			SealedCredentialBundle: sealed,
		})
		ack := &executionv1.ControlCredentialCommitAck{CommandId: commandID}
		if commitErr == nil && credential.ValidateTransportID(versionID) == nil {
			ack.Accepted = true
			ack.VersionId = versionID
		} else {
			ack.ErrorCode = "commit_rejected"
		}
		response := &executionv1.NodeControlServiceControlResponse{
			Event: &executionv1.NodeControlServiceControlResponse_CredentialCommitAck{CredentialCommitAck: ack},
		}
		select {
		case session.outbound <- response:
		case <-session.done:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (s *nodeSession) reserveCommand(commandID string, command pendingCommand) bool {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if len(s.pendingCommands) >= s.maxPendingCommands {
		return false
	}
	if _, exists := s.pendingCommands[commandID]; exists {
		return false
	}
	s.pendingCommands[commandID] = command
	return true
}

func (s *nodeSession) command(commandID string) (pendingCommand, bool) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	command, exists := s.pendingCommands[commandID]
	return command, exists
}

func (s *nodeSession) releaseCommand(commandID string) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	delete(s.pendingCommands, commandID)
}

func (s *nodeSession) beginCredentialCommit(commit *executionv1.ControlCredentialCommit) (pendingCommand, error) {
	if commit == nil || credential.ValidateTransportID(commit.GetCommandId()) != nil ||
		credential.ValidateTransportID(commit.GetAccountBinding()) != nil || credential.ValidateTransportID(commit.GetSlotId()) != nil ||
		commit.GetExecutionEpoch() == 0 || credential.ValidateTransportID(commit.GetCredentialLeaseId()) != nil ||
		credential.ValidateTransportID(commit.GetProxyLeaseId()) != nil || len(commit.GetSealedCredentialBundle()) == 0 ||
		len(commit.GetSealedCredentialBundle()) > maxCredentialBundleBytes {
		return pendingCommand{}, errors.New("credential commit is invalid")
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	pending, exists := s.pendingCommands[commit.GetCommandId()]
	if !exists || pending.kind != pendingSecureActivation || pending.commitStarted || pending.slotID != commit.GetSlotId() ||
		pending.executionEpoch != commit.GetExecutionEpoch() || pending.accountBinding != commit.GetAccountBinding() ||
		pending.credentialLeaseID != commit.GetCredentialLeaseId() || pending.proxyLeaseID != commit.GetProxyLeaseId() {
		return pendingCommand{}, errors.New("credential commit binding is invalid")
	}
	pending.commitStarted = true
	s.pendingCommands[commit.GetCommandId()] = pending
	return pending, nil
}

func (s *nodeSession) supportsSecureActivation() bool {
	if s == nil || s.protocolMinor < 1 {
		return false
	}
	_, supported := s.capabilities[secureActivationCapability]
	return supported
}

func validateProtocol(version *executionv1.ProtocolVersion) error {
	if version == nil || version.GetMajor() != CurrentProtocolMajor || version.GetMinor() > CurrentProtocolMinor ||
		(CurrentProtocolMinor > 0 && version.GetMinor()+1 < CurrentProtocolMinor) {
		return status.Errorf(codes.FailedPrecondition, "unsupported node protocol version; server supports %d.%d", CurrentProtocolMajor, CurrentProtocolMinor)
	}
	return nil
}

func validateMetadata(labels map[string]string, capabilities []string) error {
	if len(labels) > 32 {
		return errors.New("node labels exceed limit")
	}
	for key, value := range labels {
		if !metadataKeyPattern.MatchString(key) || len(value) > 128 || containsSensitiveWord(key) || containsSensitiveWord(value) {
			return errors.New("node label is invalid")
		}
	}
	if len(capabilities) > 64 {
		return errors.New("node capabilities exceed limit")
	}
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !capabilityPattern.MatchString(capability) || containsSensitiveWord(capability) {
			return errors.New("node capability is invalid")
		}
		if _, exists := seen[capability]; exists {
			return errors.New("node capabilities contain duplicates")
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func capacityFromProto(capacity *executionv1.Capacity) (store.Capacity, error) {
	if capacity == nil {
		return store.Capacity{}, errors.New("node capacity is required")
	}
	value := store.Capacity{
		MaxSlots: capacity.GetMaxSlots(), MaxActiveCLI: capacity.GetMaxActiveCli(),
		MaxActiveAPI: capacity.GetMaxActiveApi(), MaxActiveTotal: capacity.GetMaxActiveTotal(),
		AllocatableCPUMillis:   capacity.GetAllocatableCpuMillis(),
		AllocatableMemoryBytes: capacity.GetAllocatableMemoryBytes(),
	}
	if err := value.Validate(); err != nil {
		return store.Capacity{}, err
	}
	return value, nil
}

func validateSlotObservation(slot *executionv1.SlotObservation) error {
	if slot == nil || slot.GetSlotId() == "" || len(slot.GetSlotId()) > 128 || slot.GetExecutionEpoch() == 0 ||
		len(slot.GetProviderRef()) > 255 || slot.GetActualState() == "" || len(slot.GetActualState()) > 32 ||
		len(slot.GetReason()) > 1024 || (slot.GetImageDigest() != "" && !imageDigestPattern.MatchString(slot.GetImageDigest())) ||
		containsSensitiveWord(slot.GetProviderRef()) || containsSensitiveWord(slot.GetReason()) {
		return errors.New("slot observation is invalid")
	}
	return nil
}

func validateControlResponse(response *executionv1.NodeControlServiceControlResponse) error {
	if response == nil {
		return errors.New("control response is required")
	}
	if command := response.GetSlotCommand(); command != nil {
		if command.GetCommandId() == "" || len(command.GetCommandId()) > 128 || command.GetAction() == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_UNSPECIFIED ||
			command.GetSlotId() == "" || len(command.GetSlotId()) > 128 || command.GetAccountId() == "" || len(command.GetAccountId()) > 128 ||
			command.GetExecutionEpoch() == 0 || (command.GetImageDigest() != "" && !imageDigestPattern.MatchString(command.GetImageDigest())) {
			return errors.New("slot command is invalid")
		}
		if command.GetDeadline() == nil || command.GetDeadline().CheckValid() != nil {
			return errors.New("slot command deadline is invalid")
		}
		if len(command.GetMetadata()) > 16 {
			return errors.New("slot command metadata exceeds limit")
		}
		total := 0
		for key, value := range command.GetMetadata() {
			total += len(key) + len(value)
			if !metadataKeyPattern.MatchString(key) || len(value) > 512 || containsSensitiveWord(key) || containsSensitiveWord(value) {
				return errors.New("slot command metadata contains an invalid or sensitive value")
			}
		}
		if total > 4096 {
			return errors.New("slot command metadata exceeds size limit")
		}
		return nil
	}
	if revoke := response.GetRevokeEpoch(); revoke != nil {
		if revoke.GetCommandId() == "" || len(revoke.GetCommandId()) > 128 || revoke.GetSlotId() == "" ||
			len(revoke.GetSlotId()) > 128 || revoke.GetExecutionEpoch() == 0 || len(revoke.GetReason()) > 1024 || containsSensitiveWord(revoke.GetReason()) {
			return errors.New("revoke epoch command is invalid")
		}
		return nil
	}
	if command := response.GetCredentialKeyCommand(); command != nil {
		if err := validateSecureCommandBinding(
			command.GetCommandId(), command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(), command.GetDeadline(),
		); err != nil {
			return errors.New("credential-key command is invalid")
		}
		return nil
	}
	if command := response.GetSecureActivationCommand(); command != nil {
		if err := validateSecureCommandBinding(
			command.GetCommandId(), command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(), command.GetDeadline(),
		); err != nil || credential.ValidateTransportID(command.GetCredentialLeaseId()) != nil ||
			credential.ValidateTransportID(command.GetProxyLeaseId()) != nil || len(command.GetEncryptedCredentialBundle()) == 0 ||
			len(command.GetEncryptedCredentialBundle()) > maxCredentialBundleBytes {
			return errors.New("secure activation command is invalid")
		}
		return nil
	}
	return errors.New("control response event is required")
}

func validateSecureCommandBinding(commandID, slotID, accountID string, epoch uint64, imageDigest string, deadline *timestamppb.Timestamp) error {
	if credential.ValidateTransportID(commandID) != nil || credential.ValidateTransportID(slotID) != nil ||
		credential.ValidateTransportID(accountID) != nil || epoch == 0 || !imageDigestPattern.MatchString(imageDigest) ||
		deadline == nil || deadline.CheckValid() != nil {
		return errors.New("secure control command binding is invalid")
	}
	return nil
}

func controlCommandID(response *executionv1.NodeControlServiceControlResponse) string {
	if command := response.GetSlotCommand(); command != nil {
		return command.GetCommandId()
	}
	if revoke := response.GetRevokeEpoch(); revoke != nil {
		return revoke.GetCommandId()
	}
	if command := response.GetCredentialKeyCommand(); command != nil {
		return command.GetCommandId()
	}
	if command := response.GetSecureActivationCommand(); command != nil {
		return command.GetCommandId()
	}
	return ""
}

func pendingFromResponse(response *executionv1.NodeControlServiceControlResponse) pendingCommand {
	if command := response.GetSlotCommand(); command != nil {
		return pendingCommand{kind: pendingSlotCommand, slotID: command.GetSlotId(), executionEpoch: command.GetExecutionEpoch()}
	}
	if revoke := response.GetRevokeEpoch(); revoke != nil {
		return pendingCommand{kind: pendingEpochRevocation, slotID: revoke.GetSlotId(), executionEpoch: revoke.GetExecutionEpoch()}
	}
	if command := response.GetCredentialKeyCommand(); command != nil {
		return pendingCommand{
			kind: pendingCredentialKey, slotID: command.GetSlotId(), executionEpoch: command.GetExecutionEpoch(), imageDigest: command.GetImageDigest(),
			deadline: command.GetDeadline().AsTime(),
		}
	}
	if command := response.GetSecureActivationCommand(); command != nil {
		return pendingCommand{
			kind: pendingSecureActivation, slotID: command.GetSlotId(), executionEpoch: command.GetExecutionEpoch(),
			accountBinding: provider.RuntimeAccountID(command.GetAccountId()), credentialLeaseID: command.GetCredentialLeaseId(),
			proxyLeaseID: command.GetProxyLeaseId(), imageDigest: command.GetImageDigest(), deadline: command.GetDeadline().AsTime(),
		}
	}
	return pendingCommand{}
}

func controlDeadline(response *executionv1.NodeControlServiceControlResponse) time.Time {
	if command := response.GetSlotCommand(); command != nil && command.GetDeadline() != nil {
		return command.GetDeadline().AsTime()
	}
	if command := response.GetCredentialKeyCommand(); command != nil && command.GetDeadline() != nil {
		return command.GetDeadline().AsTime()
	}
	if command := response.GetSecureActivationCommand(); command != nil && command.GetDeadline() != nil {
		return command.GetDeadline().AsTime()
	}
	return time.Time{}
}

func certificateRecord(nodeID string, issued pki.IssuedCertificate, createdAt time.Time) store.Certificate {
	return store.Certificate{
		SerialNumber: issued.SerialNumber, NodeID: nodeID, CertificateSHA256: issued.CertificateSHA256,
		PublicKeySHA256: issued.PublicKeySHA256, Status: "active", NotBefore: issued.Certificate.NotBefore,
		ExpiresAt: issued.Certificate.NotAfter, CreatedAt: createdAt.UTC(),
	}
}

func ServerTLSConfig(certificate tls.Certificate, authority *pki.Authority) (*tls.Config, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil || authority == nil {
		return nil, errors.New("server certificate and authority are required")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate}, ClientCAs: authority.CertificatePool(),
		ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS13,
	}, nil
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate enrollment token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func resetTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func containsSensitiveWord(value string) bool {
	normalized := strings.ToLower(value)
	for _, word := range sensitiveWords {
		if strings.Contains(normalized, word) {
			return true
		}
	}
	return false
}

func sanitizeError(value string) string {
	if containsSensitiveWord(value) {
		return "redacted internal error"
	}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 1024 {
		runes = runes[:1024]
	}
	return string(runes)
}

func sanitizeReasonCode(value string) string {
	if containsSensitiveWord(value) {
		return "redacted"
	}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func capabilitySet(capabilities []string) map[string]struct{} {
	result := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		result[capability] = struct{}{}
	}
	return result
}

func zeroControlBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
