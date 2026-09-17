package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

// This is an explicit Docker inspect fixture, including fields that the old
// decoder discarded. Container.Image is the local config ID, not the registry
// manifest digest in Config.Image and the deployment label.
func sandboxFixture(t *testing.T) (Container, Network) {
	t.Helper()
	spec := dockerSpec()
	name := containerName(spec.SlotID)
	networkName := "execution-net-" + strings.TrimPrefix(name, "execution-slot-")
	containerID, imageID, networkID := strings.Repeat("c", 64), "sha256:"+strings.Repeat("b", 64), strings.Repeat("d", 64)
	raw := fmt.Sprintf(`{
		"Id":%q,"Name":%q,"Created":"2033-05-18T03:33:20Z","Image":%q,
		"Config":{"Image":%q,"Hostname":%q,"User":"65532:65532","Env":[
			"EXECUTION_SLOT_ID=slot/customer-1","EXECUTION_EPOCH=11","EXECUTION_RUNTIME_GENERATION=3",
			"EXECUTION_EGRESS_PROXY_URL=http://host-agent.execution.internal:18080",
			"HTTP_PROXY=http://host-agent.execution.internal:18080","HTTPS_PROXY=http://host-agent.execution.internal:18080","NO_PROXY=127.0.0.1,localhost"],"Labels":{
			"com.sub2api.execution.managed":"true","com.sub2api.execution.slot_id":%q,
			"com.sub2api.execution.account_hash":%q,"com.sub2api.execution.epoch":"11",
			"com.sub2api.execution.runtime_generation":"3",
			"com.sub2api.execution.image_digest":%q}},
		"HostConfig":{"NetworkMode":%q,"ReadonlyRootfs":true,"CapDrop":["ALL"],"CapAdd":null,
			"SecurityOpt":["no-new-privileges=true","seccomp=builtin","apparmor=docker-default"],
			"Privileged":false,"PidMode":"","IpcMode":"private","UTSMode":"","UsernsMode":"","CgroupnsMode":"private",
			"PidsLimit":128,"Memory":536870912,"NanoCpus":500000000,"Init":true,"Runtime":"runc",
			"Tmpfs":{"/tmp":"rw,noexec,nosuid,nodev,size=67108864","/run":"rw,noexec,nosuid,nodev,size=67108864"},
			"Binds":null,"Mounts":null,"VolumesFrom":null,"Devices":[],"DeviceRequests":null,"PortBindings":{},
			"ExtraHosts":["host-agent.execution.internal:172.31.0.1"]},
		"NetworkSettings":{"Ports":{},"Networks":{%q:{"NetworkID":%q,"IPAddress":"172.31.0.2","Gateway":"172.31.0.1","GlobalIPv6Address":""}}},
		"AppArmorProfile":"docker-default","Mounts":[{"Type":"tmpfs","Source":"","Destination":"/tmp","Mode":"","RW":true,"Propagation":""},
			{"Type":"tmpfs","Source":"","Destination":"/run","Mode":"","RW":true,"Propagation":""}],
		"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}
	}`, containerID, "/"+name, imageID, spec.ImageDigest, name, spec.SlotID, base.RuntimeAccountID(spec.AccountID), spec.ImageDigest, networkName, networkName, networkID)
	var container Container
	if err := json.Unmarshal([]byte(raw), &container); err != nil {
		t.Fatal(err)
	}
	raw = fmt.Sprintf(`{"Id":%q,"Name":%q,"Driver":"bridge","Internal":true,"Attachable":false,"EnableIPv6":false,
		"Labels":{"com.sub2api.execution.managed":"true","com.sub2api.execution.slot_id":%q},
		"IPAM":{"Config":[{"Subnet":"172.31.0.0/16","Gateway":"172.31.0.1"}]},
		"Containers":{%q:{"Name":%q,"IPv4Address":"172.31.0.2/16","IPv6Address":""}}}`, networkID, networkName, spec.SlotID, containerID, name)
	var network Network
	if err := json.Unmarshal([]byte(raw), &network); err != nil {
		t.Fatal(err)
	}
	return container, network
}

func sandboxImageFixture() Image {
	return Image{ID: "sha256:" + strings.Repeat("b", 64), RepoDigests: []string{dockerSpec().ImageDigest}}
}

func sandboxTestEngine(t *testing.T) *fakeEngine {
	t.Helper()
	container, network := sandboxFixture(t)
	return &fakeEngine{container: container, network: network, image: sandboxImageFixture()}
}

func TestSandboxRejectsReusedAccountMismatch(t *testing.T) {
	container, network := sandboxFixture(t)
	engine := &fakeEngine{container: container, network: network, image: sandboxImageFixture()}
	if _, err := newTestProvider(t, engine).Create(context.Background(), dockerSpec()); err != nil {
		t.Fatal(err)
	}
	container.Config.Labels[labelAccountHash] = base.RuntimeAccountID("another-account")
	if _, err := newTestProvider(t, engine).Create(context.Background(), dockerSpec()); err == nil {
		t.Fatal("reused another account's execution instance")
	}
}

func TestSandboxRejectsMissingOrAdditionalNetwork(t *testing.T) {
	for _, edge := range []string{"missing", "additional", "host"} {
		t.Run(edge, func(t *testing.T) {
			container, network := sandboxFixture(t)
			baseline := &fakeEngine{container: container, network: network, image: sandboxImageFixture()}
			if _, err := newTestProvider(t, baseline).Inspect(context.Background(), container.ID); err != nil {
				t.Fatal(err)
			}
			switch edge {
			case "missing":
				container.NetworkSettings.Networks = nil
			case "additional":
				container.NetworkSettings.Networks["outside"] = container.NetworkSettings.Networks[network.Name]
			case "host":
				container.HostConfig.NetworkMode = "host"
			}
			engine := &fakeEngine{container: container, network: network, image: sandboxImageFixture()}
			if _, err := newTestProvider(t, engine).Inspect(context.Background(), container.ID); err == nil {
				t.Fatal("accepted a runtime without its exclusive dedicated network")
			}
		})
	}
}

func TestSandboxRejectsDisabledNoNewPrivileges(t *testing.T) {
	container, network := sandboxFixture(t)
	engine := &fakeEngine{container: container, network: network, image: sandboxImageFixture()}
	if _, err := newTestProvider(t, engine).Inspect(context.Background(), container.ID); err != nil {
		t.Fatal(err)
	}
	container.HostConfig.SecurityOpt[0] = "no-new-privileges=false"
	if _, err := newTestProvider(t, engine).Inspect(context.Background(), container.ID); err == nil {
		t.Fatal("accepted disabled no-new-privileges")
	}
}

func sandboxProviderWithBootstrap(t *testing.T, engine *fakeEngine) *Provider {
	t.Helper()
	config := DefaultConfig()
	config.Now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	config.WorkerBootstrap = &WorkerBootstrap{
		NodeID: "node-sandbox", TicketPublicKey: base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
		UpstreamBaseURL: "https://api.anthropic.com", RuntimePort: 8093,
		IdentityDirectory: WorkerIdentityDirectory, RuntimeTrustFile: WorkerRuntimeTrustFile,
	}
	engine.container.Config.Env = append(engine.container.Config.Env,
		"EXECUTION_ACCOUNT_HASH="+base.RuntimeAccountID(dockerSpec().AccountID), "EXECUTION_NODE_ID=node-sandbox",
		"EXECUTION_LISTEN_ADDRESS=0.0.0.0:8093", "EXECUTION_TICKET_PUBLIC_KEY="+config.WorkerBootstrap.TicketPublicKey,
		"EXECUTION_UPSTREAM_BASE_URL=https://api.anthropic.com", "EXECUTION_IMAGE_DIGEST="+dockerSpec().ImageDigest,
		"EXECUTION_ALLOW_FAKE_ACTIVATION=false",
		"EXECUTION_IDENTITY_DIRECTORY="+WorkerIdentityDirectory, "EXECUTION_RUNTIME_TRUST_FILE="+WorkerRuntimeTrustFile,
	)
	provider, err := New(config, engine)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func assertSandboxAcceptsAllEntrypoints(t *testing.T, provider *Provider, engine *fakeEngine) {
	t.Helper()
	ctx := context.Background()
	if _, err := provider.Create(ctx, dockerSpec()); err != nil {
		t.Fatalf("positive Create: %v", err)
	}
	if status, err := provider.Inspect(ctx, engine.container.ID); err != nil || !status.Healthy {
		t.Fatalf("positive Inspect: %v", err)
	}
	if status, err := provider.InspectSlot(ctx, dockerSpec().SlotID); err != nil || !status.Healthy {
		t.Fatalf("positive InspectSlot: %v", err)
	}
	if err := provider.Start(ctx, containerName(dockerSpec().SlotID)); err != nil || engine.startedID != engine.container.ID {
		t.Fatalf("positive Start did not use verified container ID: %v", err)
	}
	if endpoint, err := provider.RuntimeEndpoint(ctx, engine.container.ID); err != nil || endpoint != "172.31.0.2:8093" {
		t.Fatalf("positive RuntimeEndpoint: %v", err)
	}
	if engine.imageInspectedRef != dockerSpec().ImageDigest {
		t.Fatal("image was not resolved using the immutable deployment reference")
	}
	engine.calls = nil
	engine.startedID = ""
}

func assertSandboxRejectsAllEntrypoints(t *testing.T, provider *Provider, engine *fakeEngine) {
	t.Helper()
	ctx := context.Background()
	checks := []func() error{
		func() error { _, err := provider.Create(ctx, dockerSpec()); return err },
		func() error { _, err := provider.Inspect(ctx, engine.container.ID); return err },
		func() error { _, err := provider.InspectSlot(ctx, dockerSpec().SlotID); return err },
		func() error { return provider.Start(ctx, engine.container.ID) },
		func() error { _, err := provider.RuntimeEndpoint(ctx, engine.container.ID); return err },
	}
	for index, check := range checks {
		if err := check(); err == nil {
			t.Fatalf("entrypoint %d accepted a drifted sandbox", index)
		}
	}
	for _, call := range engine.calls {
		if call != "ping" && call != "inspect-container" && call != "inspect-network" && call != "inspect-image" {
			t.Fatalf("rejection mutated Docker state through %s", call)
		}
	}
}

func TestSandboxDriftIsRejectedByEveryAdoptionEntrypoint(t *testing.T) {
	cases := map[string]func(*fakeEngine){
		"privileged":       func(e *fakeEngine) { e.container.HostConfig.Privileged = true },
		"cap add":          func(e *fakeEngine) { e.container.HostConfig.CapAdd = []string{"NET_ADMIN"} },
		"pid host":         func(e *fakeEngine) { e.container.HostConfig.PidMode = "host" },
		"ipc host":         func(e *fakeEngine) { e.container.HostConfig.IpcMode = "host" },
		"uts host":         func(e *fakeEngine) { e.container.HostConfig.UTSMode = "host" },
		"userns host":      func(e *fakeEngine) { e.container.HostConfig.UsernsMode = "host" },
		"cgroupns host":    func(e *fakeEngine) { e.container.HostConfig.CgroupnsMode = "host" },
		"runtime absent":   func(e *fakeEngine) { e.container.HostConfig.Runtime = "" },
		"runtime drift":    func(e *fakeEngine) { e.container.HostConfig.Runtime = "other-runtime" },
		"zero padded root": func(e *fakeEngine) { e.container.Config.User = "0000:0000" },
		"root group":       func(e *fakeEngine) { e.container.Config.User = "65532:0" },
		"nnp false":        func(e *fakeEngine) { e.container.HostConfig.SecurityOpt[0] = "no-new-privileges=false" },
		"nnp suffix":       func(e *fakeEngine) { e.container.HostConfig.SecurityOpt[0] = "no-new-privileges-bogus" },
		"duplicate nnp": func(e *fakeEngine) {
			e.container.HostConfig.SecurityOpt = append(e.container.HostConfig.SecurityOpt, "no-new-privileges=false")
		},
		"apparmor drift":   func(e *fakeEngine) { e.container.AppArmorProfile = "unconfined" },
		"weak tmpfs":       func(e *fakeEngine) { e.container.HostConfig.Tmpfs["/tmp"] = "rw" },
		"unexpected tmpfs": func(e *fakeEngine) { e.container.HostConfig.Tmpfs["/home"] = e.container.HostConfig.Tmpfs["/tmp"] },
		"bind":             func(e *fakeEngine) { e.container.HostConfig.Binds = []string{"/var/run/docker.sock:/run/docker.sock"} },
		"volumes from":     func(e *fakeEngine) { e.container.HostConfig.VolumesFrom = []string{"another-slot"} },
		"typed mount": func(e *fakeEngine) {
			e.container.HostConfig.Mounts = []json.RawMessage{json.RawMessage(`{"Type":"bind","Source":"/","Target":"/host"}`)}
		},
		"actual mount source":    func(e *fakeEngine) { e.container.Mounts[0].Source = "/host/path" },
		"actual mount type":      func(e *fakeEngine) { e.container.Mounts[0].Type = "volume" },
		"actual mount target":    func(e *fakeEngine) { e.container.Mounts[0].Destination = "/home" },
		"actual duplicate mount": func(e *fakeEngine) { e.container.Mounts = append(e.container.Mounts, e.container.Mounts[0]) },
		"device": func(e *fakeEngine) {
			e.container.HostConfig.Devices = []json.RawMessage{json.RawMessage(`{"PathOnHost":"/dev/kvm"}`)}
		},
		"device request": func(e *fakeEngine) {
			e.container.HostConfig.DeviceRequests = []json.RawMessage{json.RawMessage(`{"Count":-1}`)}
		},
		"device rule": func(e *fakeEngine) { e.container.HostConfig.DeviceCgroupRules = []string{"a *:* rwm"} },
		"publish all": func(e *fakeEngine) { e.container.HostConfig.PublishAllPorts = true },
		"published port": func(e *fakeEngine) {
			e.container.NetworkSettings.Ports["8093/tcp"] = []PortBinding{{HostIP: "0.0.0.0", HostPort: "8093"}}
		},
		"missing account hash": func(e *fakeEngine) { delete(e.container.Config.Labels, labelAccountHash) },
		"bootstrap account": func(e *fakeEngine) {
			replaceSandboxEnv(&e.container, "EXECUTION_ACCOUNT_HASH", strings.Repeat("f", 32))
		},
		"bootstrap node": func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_NODE_ID", "another-node") },
		"bootstrap image": func(e *fakeEngine) {
			replaceSandboxEnv(&e.container, "EXECUTION_IMAGE_DIGEST", "sha256:"+strings.Repeat("f", 64))
		},
		"bootstrap public key": func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_TICKET_PUBLIC_KEY", "another-key") },
		"bootstrap listen":     func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_LISTEN_ADDRESS", "0.0.0.0:8999") },
		"bootstrap upstream": func(e *fakeEngine) {
			replaceSandboxEnv(&e.container, "EXECUTION_UPSTREAM_BASE_URL", "https://wrong.invalid")
		},
		"bootstrap fake mode":          func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_ALLOW_FAKE_ACTIVATION", "true") },
		"bootstrap identity directory": func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_IDENTITY_DIRECTORY", "/tmp/shared") },
		"bootstrap TLS root": func(e *fakeEngine) {
			replaceSandboxEnv(&e.container, "EXECUTION_RUNTIME_TRUST_FILE", "/tmp/caller-ca.pem")
		},
		"runtime generation environment": func(e *fakeEngine) { replaceSandboxEnv(&e.container, "EXECUTION_RUNTIME_GENERATION", "4") },
		"runtime generation label":       func(e *fakeEngine) { e.container.Config.Labels[labelRuntimeGeneration] = "04" },
		"duplicate environment":          func(e *fakeEngine) { e.container.Config.Env = append(e.container.Config.Env, "EXECUTION_EPOCH=11") },
		"secret environment": func(e *fakeEngine) {
			e.container.Config.Env = append(e.container.Config.Env, "ACCESS_TOKEN=synthetic-forbidden")
		},
		"network absent": func(e *fakeEngine) { e.container.NetworkSettings.Networks = nil },
		"network additional": func(e *fakeEngine) {
			e.container.NetworkSettings.Networks["outside"] = e.container.NetworkSettings.Networks[e.network.Name]
		},
		"network wrong":          func(e *fakeEngine) { e.container.HostConfig.NetworkMode = "host" },
		"network external":       func(e *fakeEngine) { e.network.Internal = false },
		"network driver":         func(e *fakeEngine) { e.network.Driver = "overlay" },
		"network attachable":     func(e *fakeEngine) { e.network.Attachable = true },
		"network ipv6":           func(e *fakeEngine) { e.network.EnableIPv6 = true },
		"network owner":          func(e *fakeEngine) { e.network.Labels[labelSlotID] = "other-slot" },
		"network name":           func(e *fakeEngine) { e.network.Name = "other-network" },
		"network id":             func(e *fakeEngine) { e.network.ID = "another-network-id" },
		"network member missing": func(e *fakeEngine) { e.network.Containers = nil },
		"network extra member":   func(e *fakeEngine) { e.network.Containers["another-id"] = NetworkContainer{Name: "another-container"} },
		"network lookup error":   func(e *fakeEngine) { e.networkInspectError = errors.New("synthetic network unavailable") },
		"gateway mapping": func(e *fakeEngine) {
			e.container.HostConfig.ExtraHosts = []string{"host-agent.execution.internal:172.31.0.99"}
		},
		"actual image":          func(e *fakeEngine) { e.container.Image = "sha256:" + strings.Repeat("f", 64) },
		"image reference":       func(e *fakeEngine) { e.container.Config.Image = "worker:latest" },
		"image label":           func(e *fakeEngine) { e.container.Config.Labels[labelImageDigest] = "sha256:" + strings.Repeat("f", 64) },
		"image lookup error":    func(e *fakeEngine) { e.imageInspectError = errors.New("synthetic image unavailable") },
		"image lookup mismatch": func(e *fakeEngine) { e.image.ID = "sha256:" + strings.Repeat("f", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			provider := sandboxProviderWithBootstrap(t, engine)
			assertSandboxAcceptsAllEntrypoints(t, provider, engine)
			mutate(engine)
			assertSandboxRejectsAllEntrypoints(t, provider, engine)
		})
	}
}

func replaceSandboxEnv(container *Container, name, value string) {
	for index, raw := range container.Config.Env {
		if strings.HasPrefix(raw, name+"=") {
			container.Config.Env[index] = name + "=" + value
			return
		}
	}
	panic("fixture environment field is absent")
}

func TestSandboxStoppedBeforeFirstStartHasNoRuntimeEndpoint(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := sandboxProviderWithBootstrap(t, engine)
	assertSandboxAcceptsAllEntrypoints(t, provider, engine)
	engine.container.State.Running, engine.container.State.Status = false, "created"
	engine.container.NetworkSettings.Networks[engine.network.Name] = NetworkEndpoint{}
	engine.network.Containers = nil
	ctx := context.Background()
	if _, err := provider.Create(ctx, dockerSpec()); err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Inspect(ctx, engine.container.ID); err != nil || status.Healthy {
		t.Fatalf("stopped state: %v", err)
	}
	if err := provider.Start(ctx, engine.container.ID); err != nil {
		t.Fatal(err)
	}
	if endpoint, err := provider.RuntimeEndpoint(ctx, engine.container.ID); err == nil || endpoint != "" {
		t.Fatal("stopped runtime advertised an endpoint")
	}
}

func TestSandboxVerifiesImageIDWithoutConfusingRegistryManifest(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := newTestProvider(t, engine)
	if engine.container.Image == engine.container.Config.Image {
		t.Fatal("fixture must distinguish config ID and manifest reference")
	}
	if _, err := provider.Inspect(context.Background(), engine.container.ID); err != nil {
		t.Fatal(err)
	}
	engine.container.Config.Image, engine.container.Config.Labels[labelImageDigest] = engine.image.ID, engine.image.ID
	engine.image.RepoDigests = nil // Locally built immutable images have no repository digest.
	if _, err := provider.Inspect(context.Background(), engine.container.ID); err != nil {
		t.Fatal(err)
	}
	if engine.imageInspectedRef != engine.image.ID {
		t.Fatal("bare image ID was not resolved directly")
	}
}

func TestSandboxAcceptsEngineOmittingTmpfsProjection(t *testing.T) {
	engine := sandboxTestEngine(t)
	provider := sandboxProviderWithBootstrap(t, engine)
	assertSandboxAcceptsAllEntrypoints(t, provider, engine)
	engine.container.Mounts = nil
	assertSandboxAcceptsAllEntrypoints(t, provider, engine)
}

func TestSandboxReuseMatchesRequestedSecurityAndResources(t *testing.T) {
	for name, mutate := range map[string]func(*base.SlotSpec){
		"seccomp":  func(s *base.SlotSpec) { s.Security.SeccompProfile = "strict-seccomp" },
		"apparmor": func(s *base.SlotSpec) { s.Security.AppArmorProfile = "strict-apparmor" },
		"tmpfs":    func(s *base.SlotSpec) { s.Resources.TmpfsBytes *= 2 },
		"memory":   func(s *base.SlotSpec) { s.Resources.MemoryBytes *= 2 },
		"pids":     func(s *base.SlotSpec) { s.Resources.PIDs *= 2 },
		"cpu":      func(s *base.SlotSpec) { s.Resources.CPUMilli *= 2 },
		"user":     func(s *base.SlotSpec) { s.Security.RunAsUser++ },
		"proxy":    func(s *base.SlotSpec) { s.Network.EgressProxyEndpoint = "http://host-agent.execution.internal:18081" },
	} {
		t.Run(name, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			config := DefaultConfig()
			config.AllowedSeccompProfiles = append(config.AllowedSeccompProfiles, "strict-seccomp")
			config.AllowedAppArmorProfiles = append(config.AllowedAppArmorProfiles, "strict-apparmor")
			provider, err := New(config, engine)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Create(context.Background(), dockerSpec()); err != nil {
				t.Fatal(err)
			}
			spec := dockerSpec()
			mutate(&spec)
			engine.calls = nil
			if _, err := provider.Create(context.Background(), spec); err == nil {
				t.Fatal("reused a different requested sandbox configuration")
			}
			for _, call := range engine.calls {
				if call != "ping" && call != "inspect-container" && call != "inspect-network" && call != "inspect-image" {
					t.Fatal("mismatch changed Docker state")
				}
			}
		})
	}
}

func TestSandboxCreateRefusesAlreadyOccupiedDedicatedNetwork(t *testing.T) {
	engine := sandboxTestEngine(t)
	engine.inspectError = notFound()
	engine.network.Containers = map[string]NetworkContainer{"foreign-id": {Name: "foreign-container", IPv4Address: "172.31.0.3/16"}}
	if _, err := newTestProvider(t, engine).Create(context.Background(), dockerSpec()); err == nil {
		t.Fatal("created a container on an occupied network")
	}
	for _, call := range engine.calls {
		if call != "ping" && call != "inspect-container" && call != "inspect-network" {
			t.Fatalf("occupied network was mutated: %s", call)
		}
	}
}

func TestSandboxEndpointDriftIsDenied(t *testing.T) {
	for _, edge := range []string{"network-id", "ip", "gateway", "ipv6", "member-ip", "member-name", "member-ipv6", "subnet"} {
		t.Run(edge, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			provider := sandboxProviderWithBootstrap(t, engine)
			assertSandboxAcceptsAllEntrypoints(t, provider, engine)
			endpoint := engine.container.NetworkSettings.Networks[engine.network.Name]
			member := engine.network.Containers[engine.container.ID]
			switch edge {
			case "network-id":
				endpoint.NetworkID = "wrong-network"
			case "ip":
				endpoint.IPAddress = "192.168.4.2"
			case "gateway":
				endpoint.Gateway = "172.31.0.9"
			case "ipv6":
				endpoint.GlobalIPv6Address = "fd00::2"
			case "member-ip":
				member.IPv4Address = "172.31.0.9/16"
			case "member-name":
				member.Name = "another-container"
			case "member-ipv6":
				member.IPv6Address = "fd00::2/64"
			case "subnet":
				engine.network.IPAM.Config[0].Subnet = "192.168.4.0/24"
			}
			engine.container.NetworkSettings.Networks[engine.network.Name] = endpoint
			engine.network.Containers[engine.container.ID] = member
			assertSandboxRejectsAllEntrypoints(t, provider, engine)
		})
	}
}
