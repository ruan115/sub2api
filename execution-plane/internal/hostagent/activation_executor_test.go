package hostagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeActivationRuntimeStarter struct {
	runtime activationRuntime
	spec    provider.SlotSpec
	err     error
}

func (s *fakeActivationRuntimeStarter) StartRuntime(_ context.Context, spec provider.SlotSpec) (activationRuntime, error) {
	s.spec = spec
	if s.err != nil {
		return nil, s.err
	}
	return s.runtime, nil
}

type fakeActivationRuntime struct {
	key             CredentialTransportKey
	slotID          string
	epoch           uint64
	imageDigest     string
	activation      ActivationLease
	commit          worker.SealedCredentialCommitRequest
	committedID     string
	closed          int
	activationCalls int
	activationFail  bool
}

func (r *fakeActivationRuntime) CredentialTransportKey(context.Context) (CredentialTransportKey, error) {
	return CredentialTransportKey{KeyID: r.key.KeyID, PublicKey: append([]byte(nil), r.key.PublicKey...)}, nil
}

func (r *fakeActivationRuntime) ActivateSecure(ctx context.Context, activation ActivationLease, sink worker.SealedCredentialSink) ([]executionv1.ExecutionMode, error) {
	r.activationCalls++
	if r.activationFail {
		return nil, errors.New("worker activation failed")
	}
	r.activation = ActivationLease{
		CredentialLeaseID: activation.CredentialLeaseID, ProxyLeaseID: activation.ProxyLeaseID,
		EncryptedCredentialBundle: append([]byte(nil), activation.EncryptedCredentialBundle...),
	}
	r.commit = worker.SealedCredentialCommitRequest{
		AccountBinding: provider.RuntimeAccountID("account-10380"), SlotID: r.slotID, ExecutionEpoch: r.epoch,
		CredentialLeaseID: activation.CredentialLeaseID, ProxyLeaseID: activation.ProxyLeaseID,
		SealedCredentialBundle: []byte("sealed-rotation"),
	}
	versionID, err := sink.CommitSealedCredential(ctx, r.commit)
	if err != nil {
		return nil, err
	}
	r.committedID = versionID
	return []executionv1.ExecutionMode{executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API}, nil
}

func (r *fakeActivationRuntime) Health(context.Context) (*executionv1.HealthResponse, error) {
	return &executionv1.HealthResponse{SlotId: r.slotID, ExecutionEpoch: r.epoch, ImageDigest: r.imageDigest}, nil
}

func (r *fakeActivationRuntime) Close() error {
	r.closed++
	return nil
}

type capturingActivationSink struct {
	request worker.SealedCredentialCommitRequest
}

func (s *capturingActivationSink) CommitSealedCredential(_ context.Context, request worker.SealedCredentialCommitRequest) (string, error) {
	s.request = request
	s.request.SealedCredentialBundle = append([]byte(nil), request.SealedCredentialBundle...)
	return "55555555-6666-4777-8888-999999999999", nil
}

func TestRuntimeActivationExecutorGetsProcessKeyAndActivatesThroughSink(t *testing.T) {
	now := time.Now().UTC()
	resources := provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 256 << 20}
	security := provider.SecurityPolicy{
		RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
		SeccompProfile: "worker", AppArmorProfile: "worker",
	}
	network := provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080"}
	imageDigest := "sha256:" + strings.Repeat("f", 64)
	implementation := providerfake.New()
	instance, err := implementation.Create(context.Background(), provider.SlotSpec{
		SlotID: "slot-10380", AccountID: "account-10380", Epoch: 19, ImageDigest: imageDigest,
		Resources: resources, Security: security, Network: network,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.Start(context.Background(), instance.ProviderRef); err != nil {
		t.Fatal(err)
	}
	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeActivationRuntime{
		key: CredentialTransportKey{KeyID: keyID, PublicKey: publicKey}, slotID: "slot-10380", epoch: 19, imageDigest: imageDigest,
	}
	starter := &fakeActivationRuntimeStarter{runtime: runtime}
	executor := &RuntimeActivationExecutor{
		starter: starter, inspector: implementation, resources: resources, security: security, network: network,
	}
	keyResult := executor.CredentialTransportKey(context.Background(), &executionv1.CredentialKeyCommand{
		CommandId: "cmd-key", SlotId: "slot-10380", AccountId: "account-10380", ExecutionEpoch: 19,
		ImageDigest: imageDigest, Deadline: timestamppb.New(now.Add(time.Minute)),
	})
	if !keyResult.GetSucceeded() || credential.ValidateRecipientKey(keyResult.GetCredentialTransportKey().GetKeyId(), keyResult.GetCredentialTransportKey().GetPublicKey()) != nil ||
		starter.spec.AccountID != "account-10380" || runtime.closed != 1 {
		t.Fatalf("credential-key result/spec/closed = %+v/%+v/%d", keyResult, starter.spec, runtime.closed)
	}
	bundle := []byte("process-bound-encrypted-bundle")
	sink := &capturingActivationSink{}
	activationResult := executor.SecureActivate(context.Background(), &executionv1.SecureActivationCommand{
		CommandId: "cmd-activate", SlotId: "slot-10380", AccountId: "account-10380", ExecutionEpoch: 19,
		ImageDigest: imageDigest, CredentialLeaseId: "lease-10380", ProxyLeaseId: "proxy-10380",
		EncryptedCredentialBundle: bundle, Deadline: timestamppb.New(now.Add(time.Minute)),
	}, sink)
	if !activationResult.GetSucceeded() || runtime.closed != 2 || runtime.committedID != "55555555-6666-4777-8888-999999999999" ||
		!bytes.Equal(runtime.activation.EncryptedCredentialBundle, bundle) || !bytes.Equal(sink.request.SealedCredentialBundle, []byte("sealed-rotation")) {
		t.Fatalf("activation result/runtime/sink = %+v/%+v/%+v", activationResult, runtime, sink.request)
	}
}

func TestRuntimeActivationExecutorFailsClosedWithoutSinkOrOnImageMismatch(t *testing.T) {
	now := time.Now().UTC()
	command := &executionv1.SecureActivationCommand{
		CommandId: "cmd-activate", SlotId: "slot-10380", AccountId: "account-10380", ExecutionEpoch: 19,
		ImageDigest: "sha256:" + strings.Repeat("f", 64), CredentialLeaseId: "lease-10380", ProxyLeaseId: "proxy-10380",
		EncryptedCredentialBundle: []byte("ciphertext"), Deadline: timestamppb.New(now.Add(time.Minute)),
	}
	result := (&RuntimeActivationExecutor{}).SecureActivate(context.Background(), command, nil)
	if result.GetSucceeded() || result.GetErrorCode() != "invalid_command" {
		t.Fatalf("missing sink result = %+v", result)
	}

	resources := provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 256 << 20}
	security := provider.SecurityPolicy{
		RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
		SeccompProfile: "worker", AppArmorProfile: "worker",
	}
	network := provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080"}
	implementation := providerfake.New()
	instance, err := implementation.Create(context.Background(), provider.SlotSpec{
		SlotID: "slot-10380", AccountID: "account-10380", Epoch: 19,
		ImageDigest: "sha256:" + strings.Repeat("e", 64), Resources: resources, Security: security, Network: network,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.Start(context.Background(), instance.ProviderRef); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeActivationRuntime{slotID: "slot-10380", epoch: 19, imageDigest: command.GetImageDigest()}
	executor := &RuntimeActivationExecutor{
		starter: &fakeActivationRuntimeStarter{runtime: runtime}, inspector: implementation,
		resources: resources, security: security, network: network,
	}
	result = executor.SecureActivate(context.Background(), command, &capturingActivationSink{})
	if result.GetSucceeded() || result.GetErrorCode() != "secure_activation_failed" || runtime.activationCalls != 0 || runtime.closed != 1 {
		t.Fatalf("image mismatch result/calls/closed = %+v/%d/%d", result, runtime.activationCalls, runtime.closed)
	}
}
