package providerlifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
)

type dualEngine struct {
	mu         sync.Mutex
	imageID    string
	imageRef   string
	next       int
	networks   map[string]docker.Network
	containers map[string]docker.Container
	requests   []docker.CreateContainerRequest
	removed    []string
}

func newDualEngine(imageRef string) *dualEngine {
	sum := sha256.Sum256([]byte(ExperimentName + "/local-config"))
	return &dualEngine{
		imageID:    "sha256:" + hex.EncodeToString(sum[:]),
		imageRef:   imageRef,
		networks:   map[string]docker.Network{},
		containers: map[string]docker.Container{},
	}
}

func notFound() error {
	return &docker.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (e *dualEngine) Ping(context.Context) error { return nil }

func (e *dualEngine) InspectNetwork(_ context.Context, id string) (docker.Network, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, network := range e.networks {
		if network.Name == id || network.ID == id {
			return cloneNetwork(network), nil
		}
	}
	return docker.Network{}, notFound()
}

func (e *dualEngine) CreateNetwork(_ context.Context, request docker.CreateNetworkRequest) (docker.CreateNetworkResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	id := strings.Repeat(strconv.FormatInt(int64(e.next), 16), 64)[:64]
	octet := e.next + 1
	network := docker.Network{
		ID: id, Name: request.Name, Internal: request.Internal, Driver: request.Driver,
		Attachable: request.Attachable, EnableIPv6: false, Labels: cloneMap(request.Labels),
		IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMConfig{{
			Subnet: fmt.Sprintf("172.30.%d.0/24", octet), Gateway: fmt.Sprintf("172.30.%d.1", octet),
		}}},
		Containers: map[string]docker.NetworkContainer{},
	}
	e.networks[request.Name] = network
	return docker.CreateNetworkResponse{ID: id}, nil
}

func (e *dualEngine) RemoveNetwork(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, network := range e.networks {
		if network.Name == id || network.ID == id {
			delete(e.networks, name)
			e.removed = append(e.removed, network.ID)
			return nil
		}
	}
	return notFound()
}

func (e *dualEngine) CreateContainer(_ context.Context, name string, request docker.CreateContainerRequest) (docker.CreateContainerResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, request)
	e.next++
	id := strings.Repeat(strconv.FormatInt(int64(e.next+8), 16), 64)[:64]
	apparmor := ""
	for _, option := range request.HostConfig.SecurityOpt {
		if strings.HasPrefix(option, "apparmor=") {
			apparmor = strings.TrimPrefix(option, "apparmor=")
		}
	}
	network := e.networks[request.HostConfig.NetworkMode]
	initCopy := false
	if request.HostConfig.Init != nil {
		initCopy = *request.HostConfig.Init
		request.HostConfig.Init = &initCopy
	}
	container := docker.Container{
		ID: id, Name: "/" + name, Created: time.Unix(2_000_000_000, 0).UTC().Format(time.RFC3339),
		Image: e.imageID, AppArmorProfile: apparmor,
		HostConfig: request.HostConfig,
		State:      docker.ContainerState{Status: "created", Running: false},
	}
	container.Config.Image = request.Image
	container.Config.Hostname = request.Hostname
	container.Config.User = request.User
	container.Config.Labels = cloneMap(request.Labels)
	container.Config.Env = append([]string(nil), request.Env...)
	container.NetworkSettings.Ports = map[string][]docker.PortBinding{}
	container.NetworkSettings.Networks = map[string]docker.NetworkEndpoint{
		request.HostConfig.NetworkMode: {NetworkID: network.ID},
	}
	e.containers[name] = container
	e.containers[id] = container
	return docker.CreateContainerResponse{ID: id}, nil
}

func (e *dualEngine) InspectContainer(_ context.Context, id string) (docker.Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	container, ok := e.containers[strings.TrimPrefix(id, "/")]
	if !ok {
		return docker.Container{}, notFound()
	}
	return container, nil
}

func (e *dualEngine) InspectImage(_ context.Context, ref string) (docker.Image, error) {
	if ref != e.imageRef {
		return docker.Image{}, notFound()
	}
	return docker.Image{ID: e.imageID, RepoDigests: []string{e.imageRef}}, nil
}

func (e *dualEngine) StartContainer(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	container, ok := e.containers[id]
	if !ok {
		return notFound()
	}
	container.State.Running = true
	container.State.Status = "running"
	e.containers[id] = container
	e.containers[strings.TrimPrefix(container.Name, "/")] = container
	return nil
}

func (e *dualEngine) KillContainer(context.Context, string, string) error { return nil }
func (e *dualEngine) StopContainer(context.Context, string, time.Duration) error {
	return nil
}

func (e *dualEngine) RemoveContainer(_ context.Context, id string, _, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	container, ok := e.containers[strings.TrimPrefix(id, "/")]
	if !ok {
		return notFound()
	}
	delete(e.containers, strings.TrimPrefix(container.Name, "/"))
	delete(e.containers, container.ID)
	e.removed = append(e.removed, container.ID)
	return nil
}

func cloneMap(values map[string]string) map[string]string {
	cloned := map[string]string{}
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneNetwork(network docker.Network) docker.Network {
	network.Labels = cloneMap(network.Labels)
	if network.Containers != nil {
		cloned := map[string]docker.NetworkContainer{}
		for key, value := range network.Containers {
			cloned[key] = value
		}
		network.Containers = cloned
	}
	return network
}

func TestPlanRejectsMutableImageAndDuplicateSlots(t *testing.T) {
	if _, err := DefaultPlan("worker:latest"); err == nil {
		t.Fatal("mutable image was accepted")
	}
	plan, err := DefaultPlan(SyntheticImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	plan.Slots[1] = plan.Slots[0]
	if err := plan.Validate(); err == nil {
		t.Fatal("duplicate slots were accepted")
	}
}

func TestCreatePairUsesExclusiveInternalNetworksWithoutBinds(t *testing.T) {
	plan, err := DefaultPlan(SyntheticImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	engine := newDualEngine(plan.ImageDigest)
	runtime, err := NewProvider(engine, func() time.Time { return time.Unix(2_000_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	inv, err := CreatePair(context.Background(), runtime, engine, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(engine.requests) != 2 {
		t.Fatalf("creates=%d", len(engine.requests))
	}
	seenNets := map[string]struct{}{}
	for _, request := range engine.requests {
		if len(request.HostConfig.Binds) != 0 || len(request.HostConfig.Mounts) != 0 ||
			request.HostConfig.Memory <= 0 || request.HostConfig.MemorySwap != request.HostConfig.Memory {
			t.Fatalf("unsafe create request: %+v", request.HostConfig)
		}
		if strings.Contains(strings.Join(request.Env, " "), "account-p2") {
			t.Fatal("raw account id leaked into worker environment")
		}
		if request.HostConfig.NetworkMode == "" || strings.Contains(request.HostConfig.NetworkMode, "host") {
			t.Fatalf("network mode = %q", request.HostConfig.NetworkMode)
		}
		seenNets[request.HostConfig.NetworkMode] = struct{}{}
		listen := ""
		for _, env := range request.Env {
			if strings.HasPrefix(env, "EXECUTION_LISTEN_ADDRESS=") {
				listen = strings.TrimPrefix(env, "EXECUTION_LISTEN_ADDRESS=")
			}
		}
		if listen != "0.0.0.0:8093" {
			t.Fatalf("listen = %q", listen)
		}
	}
	if len(seenNets) != 2 {
		t.Fatalf("exclusive networks = %d", len(seenNets))
	}
	for _, item := range inv.Instances {
		network, err := engine.InspectNetwork(context.Background(), item.NetworkName)
		if err != nil || !network.Internal || network.EnableIPv6 || network.Attachable ||
			network.Labels["com.sub2api.execution.slot_id"] != item.SlotID {
			t.Fatalf("network %+v err=%v", network, err)
		}
	}
}

func TestCrossSlotValidateExistingFails(t *testing.T) {
	plan, err := DefaultPlan(SyntheticImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	engine := newDualEngine(plan.ImageDigest)
	runtime, err := NewProvider(engine, func() time.Time { return time.Unix(2_000_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	inv, err := CreatePair(context.Background(), runtime, engine, plan)
	if err != nil {
		t.Fatal(err)
	}
	specB, err := plan.Spec("b", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	instanceA := provider.Instance{
		ProviderRef: inv.Instances[0].ProviderRef, RuntimeID: inv.Instances[0].RuntimeID,
		SlotID: inv.Instances[0].SlotID, Epoch: 1, RuntimeGeneration: 1,
	}
	if err := runtime.ValidateExisting(context.Background(), instanceA, specB); err == nil {
		t.Fatal("cross-slot existing instance was accepted")
	}
	wrong := instanceA
	wrong.RuntimeID = inv.Instances[1].RuntimeID
	specA, _ := plan.Spec("a", 1, 1)
	if err := runtime.ValidateExisting(context.Background(), wrong, specA); err == nil {
		t.Fatal("replaced physical id was accepted")
	}
}

func TestCleanupRefusesUntrackedIDsAndDestroysOnlyInventory(t *testing.T) {
	plan, err := DefaultPlan(SyntheticImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	engine := newDualEngine(plan.ImageDigest)
	runtime, err := NewProvider(engine, func() time.Time { return time.Unix(2_000_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	inv, err := CreatePair(context.Background(), runtime, engine, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefuseUntracked(strings.Repeat("e", 64), inv); err == nil {
		t.Fatal("untracked cid was accepted")
	}
	if err := Cleanup(context.Background(), runtime, inv); err != nil {
		t.Fatal(err)
	}
	if len(engine.removed) < 2 {
		t.Fatalf("removed=%v", engine.removed)
	}
	for _, item := range inv.Instances {
		if _, err := engine.InspectContainer(context.Background(), item.RuntimeID); err == nil {
			t.Fatal("recorded container survived cleanup")
		}
		if _, err := engine.InspectNetwork(context.Background(), item.NetworkID); err == nil {
			t.Fatal("recorded network survived cleanup")
		}
	}
}
