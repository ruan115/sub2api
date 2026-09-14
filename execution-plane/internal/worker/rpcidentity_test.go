package worker

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type identityRecordingExecutor struct {
	mu       sync.Mutex
	executed []*executionv1.BeginExecution
	counted  []*executionv1.CountTokensRequest
}

func (e *identityRecordingExecutor) Execute(stream ExecutionStream) error {
	e.mu.Lock()
	e.executed = append(e.executed, proto.Clone(stream.Begin()).(*executionv1.BeginExecution))
	e.mu.Unlock()
	return deterministicExecutor{}.Execute(stream)
}

func (e *identityRecordingExecutor) CountTokens(ctx context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	e.mu.Lock()
	e.counted = append(e.counted, proto.Clone(request).(*executionv1.CountTokensRequest))
	e.mu.Unlock()
	return deterministicExecutor{}.CountTokens(ctx, request)
}

func (e *identityRecordingExecutor) invocations() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.executed) + len(e.counted)
}

func TestWorkerGRPCRejectsMismatchedRequestIdentityBeforeExecutor(t *testing.T) {
	cases := []struct {
		name       string
		accountID  string
		slotID     string
		epoch      uint64
		generation uint64
		code       codes.Code
		message    string
	}{
		{"wrong_account", "other-account-sensitive", "slot-1", 12, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"wrong_slot", "account-1", "other-slot-sensitive", 12, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"missing_slot", "account-1", "", 12, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"stale_epoch", "account-1", "slot-1", 11, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"future_epoch", "account-1", "slot-1", 13, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"missing_epoch", "account-1", "slot-1", 0, 1, codes.PermissionDenied, "execution request is not assigned to this worker"},
		{"missing_generation", "account-1", "slot-1", 12, 0, codes.InvalidArgument, "execution route generation is required"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor := &identityRecordingExecutor{}
			fixture := newRPCFixtureWithExecutor(t, executor)
			defer fixture.close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := fixture.client.Execute(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rawTicket := fixture.ticket(t, "messages")
			if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
				Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{Begin: &executionv1.WorkerBeginExecution{
					ExecutionTicket: rawTicket,
					Request: &executionv1.BeginExecution{
						RequestId: "synthetic-execution", AccountId: test.accountID, SlotId: test.slotID,
						ExecutionEpoch: test.epoch, RouteGeneration: test.generation,
						AnthropicRequestJson: []byte(`{"messages":[{"role":"user","content":"synthetic-body"}]}`),
					},
				}},
			}); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Recv()
			assertIdentityRejection(t, err, test.code, test.message, fixture.identity, rawTicket)
			_, err = fixture.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
				ExecutionTicket: fixture.ticket(t, "count_tokens"),
				Request: &executionv1.CountTokensRequest{
					AccountId: test.accountID, SlotId: test.slotID,
					ExecutionEpoch: test.epoch, RouteGeneration: test.generation,
					AnthropicRequestJson: []byte(`{"messages":[{"role":"user","content":"synthetic-body"}]}`),
				},
			})
			assertIdentityRejection(t, err, test.code, test.message, fixture.identity, rawTicket)
			if got := executor.invocations(); got != 0 {
				t.Fatalf("rejected routing identity reached executor %d times", got)
			}
		})
	}
}

func assertIdentityRejection(t *testing.T, err error, code codes.Code, message string, identity Identity, rawTicket string) {
	t.Helper()
	if status.Code(err) != code || status.Convert(err).Message() != message {
		t.Fatalf("unexpected identity rejection: %v", err)
	}
	for _, secret := range []string{identity.AccountID, identity.SlotID, identity.NodeID, rawTicket, "sensitive", "synthetic-body"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("identity rejection exposed request or worker metadata")
		}
	}
}

func TestWorkerGRPCValidRoutingIdentityPreservesExecuteAndCountBodies(t *testing.T) {
	executor := &identityRecordingExecutor{}
	fixture := newRPCFixtureWithExecutor(t, executor)
	defer fixture.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := []byte(`{"model":"synthetic","messages":[{"role":"user","content":"hello"}]}`)
	// These are intentionally different positive generations: the worker does
	// not own generation authority and must not pretend its ticket binds one.
	begin := &executionv1.BeginExecution{
		RequestId: "synthetic-execution", AccountId: fixture.identity.AccountID, SlotId: fixture.identity.SlotID,
		ExecutionEpoch: fixture.identity.Epoch, RouteGeneration: 3,
		Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: body,
	}
	stream, err := fixture.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{Begin: &executionv1.WorkerBeginExecution{
			ExecutionTicket: fixture.ticket(t, "messages"), Request: begin,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil || response.GetResponse().GetCompleted().GetUpstreamRequestId() != "msg_fake_worker" {
		t.Fatalf("valid execute did not complete: %v", err)
	}
	countInput := &executionv1.CountTokensRequest{
		AccountId: fixture.identity.AccountID, SlotId: fixture.identity.SlotID,
		ExecutionEpoch: fixture.identity.Epoch, RouteGeneration: 9, AnthropicRequestJson: body,
	}
	count, err := fixture.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: fixture.ticket(t, "count_tokens"), Request: countInput,
	})
	if err != nil || !bytes.Equal(count.GetResponse().GetAnthropicResponseJson(), []byte(`{"input_tokens":7}`)) {
		t.Fatalf("valid count_tokens did not complete: %v", err)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.executed) != 1 || !proto.Equal(executor.executed[0], begin) {
		t.Fatal("execute routing identity or body changed before executor")
	}
	if len(executor.counted) != 1 || !proto.Equal(executor.counted[0], countInput) {
		t.Fatal("count_tokens routing identity or body changed before executor")
	}
}
