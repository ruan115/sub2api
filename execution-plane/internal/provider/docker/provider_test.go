package docker

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

type fakeEngine struct {
	mu sync.Mutex

	network              Network
	networkInspectError  error
	networkInspectErrors []error
	createNetworkError   error
	networkRequest       CreateNetworkRequest
	container            Container
	image                Image
	imageInspectError    error
	imageInspectedRef    string
	startedID            string
	inspectError         error
	inspectErrors        []error
	createError          error
	emptyCreateResponse  bool
	createRequest        CreateContainerRequest
	createdName          string
	calls                []string
}

func (e *fakeEngine) Ping(context.Context) error {
	e.record("ping")
	return nil
}

func (e *fakeEngine) InspectNetwork(context.Context, string) (Network, error) {
	e.record("inspect-network")
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.networkInspectErrors) > 0 {
		err := e.networkInspectErrors[0]
		e.networkInspectErrors = e.networkInspectErrors[1:]
		return e.network, err
	}
	return e.network, e.networkInspectError
}

func (e *fakeEngine) CreateNetwork(_ context.Context, request CreateNetworkRequest) (CreateNetworkResponse, error) {
	e.record("create-network")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.networkRequest = request
	if e.createNetworkError == nil {
		e.network = Network{
			ID: strings.Repeat("d", 64), Name: request.Name, Internal: request.Internal, Driver: request.Driver,
			Attachable: request.Attachable, Labels: request.Labels,
			IPAM: NetworkIPAM{Config: []NetworkIPAMConfig{{Gateway: "172.31.0.1", Subnet: "172.31.0.0/16"}}},
		}
		e.networkInspectError = nil
	}
	return CreateNetworkResponse{ID: "network-id-1"}, e.createNetworkError
}

func (e *fakeEngine) RemoveNetwork(context.Context, string) error {
	e.record("remove-network")
	return nil
}

func (e *fakeEngine) CreateContainer(_ context.Context, name string, request CreateContainerRequest) (CreateContainerResponse, error) {
	e.record("create")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.createdName = name
	e.createRequest = request
	if e.emptyCreateResponse {
		return CreateContainerResponse{}, e.createError
	}
	return CreateContainerResponse{ID: "container-id-1"}, e.createError
}

func (e *fakeEngine) InspectContainer(context.Context, string) (Container, error) {
	e.record("inspect-container")
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.inspectErrors) > 0 {
		err := e.inspectErrors[0]
		e.inspectErrors = e.inspectErrors[1:]
		return e.container, err
	}
	return e.container, e.inspectError
}

func (e *fakeEngine) InspectImage(_ context.Context, ref string) (Image, error) {
	e.record("inspect-image")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.imageInspectedRef = ref
	return e.image, e.imageInspectError
}

func (e *fakeEngine) StartContainer(_ context.Context, id string) error {
	e.record("start")
	e.mu.Lock()
	e.startedID = id
	e.mu.Unlock()
	return nil
}

func (e *fakeEngine) KillContainer(_ context.Context, _, signal string) error {
	e.record("kill:" + signal)
	return nil
}

func (e *fakeEngine) StopContainer(context.Context, string, time.Duration) error {
	e.record("stop")
	return nil
}

func (e *fakeEngine) RemoveContainer(context.Context, string, bool, bool) error {
	e.record("remove")
	return nil
}

func (e *fakeEngine) record(value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, value)
}

func dockerSpec() base.SlotSpec {
	return base.SlotSpec{
		SlotID:            "slot/customer-1",
		AccountID:         "secret-account-id",
		Epoch:             11,
		RuntimeGeneration: 3,
		ImageDigest:       "registry.example/execution-worker@sha256:" + strings.Repeat("a", 64),
		Resources: base.ResourceLimits{
			CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 128 << 20,
		},
		Security: base.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true,
			DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default",
		},
		Network: base.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
	}
}

func newTestProvider(t *testing.T, engine Engine) *Provider {
	t.Helper()
	config := DefaultConfig()
	config.Now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	provider, err := New(config, engine)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func notFound() error {
	return &APIError{StatusCode: http.StatusNotFound, Message: "No such container"}
}

func TestCreateAppliesSandboxAndDoesNotExposeAccountID(t *testing.T) {
	engine := &fakeEngine{networkInspectError: notFound(), inspectError: notFound()}
	provider := newTestProvider(t, engine)
	instance, err := provider.Create(context.Background(), dockerSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(instance.ProviderRef, "execution-slot-") || instance.State != slot.StateStopped {
		t.Fatalf("unexpected instance: %+v", instance)
	}
	request := engine.createRequest
	if request.User != "65532:65532" || !request.HostConfig.ReadonlyRootfs || !strings.HasPrefix(request.HostConfig.NetworkMode, "execution-net-") {
		t.Fatalf("sandbox identity/filesystem/network missing: %+v", request)
	}
	if len(request.HostConfig.CapDrop) != 1 || request.HostConfig.CapDrop[0] != "ALL" || request.HostConfig.PidsLimit != 128 || request.HostConfig.Memory != 512<<20 || request.HostConfig.NanoCPUs != 500_000_000 {
		t.Fatalf("sandbox resource controls missing: %+v", request.HostConfig)
	}
	encoded := strings.Join(append(request.Env, request.HostConfig.SecurityOpt...), " ")
	if strings.Contains(encoded, "secret-account-id") || strings.Contains(encoded, "docker.sock") {
		t.Fatalf("runtime configuration leaked forbidden values: %s", encoded)
	}
	if request.Labels[labelAccountHash] == "" || request.Labels[labelAccountHash] == "secret-account-id" {
		t.Fatalf("account label was not hashed: %v", request.Labels)
	}
	if !engine.networkRequest.Internal || engine.networkRequest.Attachable || engine.networkRequest.Labels[labelSlotID] != dockerSpec().SlotID {
		t.Fatalf("per-slot network is not isolated: %+v", engine.networkRequest)
	}
	if len(request.HostConfig.ExtraHosts) != 1 || request.HostConfig.ExtraHosts[0] != "host-agent.execution.internal:172.31.0.1" {
		t.Fatalf("host-agent route missing: %+v", request.HostConfig.ExtraHosts)
	}
	if !contains(request.Env, "EXECUTION_EGRESS_PROXY_URL="+dockerSpec().Network.EgressProxyEndpoint) ||
		request.HostConfig.Runtime != "runc" || request.HostConfig.IpcMode != "private" || request.HostConfig.CgroupnsMode != "private" {
		t.Fatal("explicit fixed proxy or isolated namespace/runtime defaults are missing")
	}
}

func TestCreateRejectsNonInternalNetwork(t *testing.T) {
	engine := &fakeEngine{network: Network{Internal: false}, inspectError: notFound()}
	provider := newTestProvider(t, engine)
	if _, err := provider.Create(context.Background(), dockerSpec()); err == nil || !strings.Contains(err.Error(), "Internal=true") {
		t.Fatalf("expected internal network error, got %v", err)
	}
}

func TestCreateRejectsUnapprovedSecurityProfiles(t *testing.T) {
	engine := &fakeEngine{networkInspectError: notFound(), inspectError: notFound()}
	provider := newTestProvider(t, engine)
	spec := dockerSpec()
	spec.Security.SeccompProfile = "unconfined"
	if _, err := provider.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected profile allowlist error, got %v", err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("unsafe profile reached Docker Engine: %v", engine.calls)
	}
}

func healthyContainer(t *testing.T, epoch string) Container {
	t.Helper()
	container, _ := sandboxFixture(t)
	container.Config.Labels[labelEpoch] = epoch
	container.Config.Env[1] = "EXECUTION_EPOCH=" + epoch
	return container
}

func TestInspectRequiresHealthcheckAndMapsHealthy(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := newTestProvider(t, engine)
	status, err := provider.Inspect(context.Background(), "container-id-1")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Healthy || status.State != slot.StateReady || status.Epoch != 11 {
		t.Fatalf("unexpected status: %+v", status)
	}

	engine.container.State.Health = nil
	status, err = provider.Inspect(context.Background(), "container-id-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || status.State != slot.StateUnhealthy || !strings.Contains(status.Reason, "missing") {
		t.Fatalf("missing healthcheck did not fail closed: %+v", status)
	}
}

func TestInspectRejectsUnconfinedOrMountedContainer(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := newTestProvider(t, engine)
	engine.container.HostConfig.SecurityOpt = []string{"no-new-privileges=true", "seccomp=unconfined", "apparmor=docker-default"}
	if _, err := provider.Inspect(context.Background(), "container-id-1"); err == nil || !strings.Contains(err.Error(), "seccomp") {
		t.Fatalf("expected unconfined seccomp rejection, got %v", err)
	}

	engine.container = healthyContainer(t, "11")
	engine.container.HostConfig.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	if _, err := provider.Inspect(context.Background(), "container-id-1"); err == nil || !strings.Contains(err.Error(), "mount") {
		t.Fatalf("expected bind mount rejection, got %v", err)
	}
}

func TestCreateIsIdempotentButEpochFenced(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := newTestProvider(t, engine)
	instance, err := provider.Create(context.Background(), dockerSpec())
	if err != nil || !strings.HasPrefix(instance.ProviderRef, "execution-slot-") {
		t.Fatalf("idempotent create failed: instance=%+v err=%v", instance, err)
	}
	if engine.createdName != "" {
		t.Fatal("idempotent create called Engine create")
	}

	engine.container = healthyContainer(t, "10")
	if _, err := provider.Create(context.Background(), dockerSpec()); err == nil || !strings.Contains(err.Error(), "epoch 10") {
		t.Fatalf("expected epoch fence error, got %v", err)
	}
}

func TestCreateRecoversFromConcurrentNetworkAndContainerCreate(t *testing.T) {
	engine := sandboxTestEngine(t)
	engine.networkInspectErrors = []error{notFound()}
	engine.createNetworkError = &APIError{StatusCode: http.StatusConflict, Message: "network already exists"}
	engine.inspectErrors = []error{notFound()}
	engine.createError = &APIError{StatusCode: http.StatusConflict, Message: "container already exists"}
	provider := newTestProvider(t, engine)
	instance, err := provider.Create(context.Background(), dockerSpec())
	if err != nil {
		t.Fatal(err)
	}
	if instance.Epoch != 11 || instance.SlotID != dockerSpec().SlotID {
		t.Fatalf("unexpected reconciled instance: %+v", instance)
	}
	if strings.Contains(strings.Join(engine.calls, ","), "remove-network") {
		t.Fatalf("concurrent reconciliation removed an in-use network: %v", engine.calls)
	}
}

func TestCreateEmptyContainerIDCleansUpNewNetwork(t *testing.T) {
	engine := &fakeEngine{
		networkInspectError: notFound(),
		inspectError:        notFound(),
		emptyCreateResponse: true,
	}
	provider := newTestProvider(t, engine)
	if _, err := provider.Create(context.Background(), dockerSpec()); err == nil || !strings.Contains(err.Error(), "empty container id") {
		t.Fatalf("expected empty id failure, got %v", err)
	}
	if !strings.Contains(strings.Join(engine.calls, ","), "remove-network") {
		t.Fatalf("new network was not cleaned up: %v", engine.calls)
	}
}

func TestDrainStopDestroyUseNarrowEngineMethods(t *testing.T) {
	engine := &fakeEngine{}
	provider := newTestProvider(t, engine)
	ctx := context.Background()
	providerRef := containerName(dockerSpec().SlotID)
	if err := provider.Drain(ctx, providerRef, time.Unix(2_000_000_100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := provider.Stop(ctx, providerRef); err != nil {
		t.Fatal(err)
	}
	if err := provider.Destroy(ctx, providerRef); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(engine.calls, ",")
	if joined != "kill:USR1,stop,remove,remove-network" {
		t.Fatalf("unexpected lifecycle calls: %s", joined)
	}
}

func hexSuffix(slotID string) string {
	name := containerName(slotID)
	parts := strings.Split(name, "-")
	return parts[len(parts)-1]
}
