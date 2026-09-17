package docker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

type existingRecordingEngine struct {
	*fakeEngine
	refs []string
	hook func(string)
}

func (e *existingRecordingEngine) InspectContainer(ctx context.Context, ref string) (Container, error) {
	e.refs = append(e.refs, ref)
	c, err := e.fakeEngine.InspectContainer(ctx, ref)
	if e.hook != nil {
		e.hook("container")
	}
	return c, err
}
func (e *existingRecordingEngine) InspectImage(ctx context.Context, ref string) (Image, error) {
	i, err := e.fakeEngine.InspectImage(ctx, ref)
	if e.hook != nil {
		e.hook("image")
	}
	return i, err
}
func (e *existingRecordingEngine) InspectNetwork(ctx context.Context, ref string) (Network, error) {
	n, err := e.fakeEngine.InspectNetwork(ctx, ref)
	if e.hook != nil {
		e.hook("network")
	}
	return n, err
}

func existingFixture(t *testing.T) (*Provider, *existingRecordingEngine, base.Instance, base.SlotSpec) {
	t.Helper()
	engine := &existingRecordingEngine{fakeEngine: sandboxTestEngine(t)}
	spec := dockerSpec()
	expected := base.Instance{ProviderRef: containerName(spec.SlotID), RuntimeID: engine.container.ID, SlotID: spec.SlotID, Epoch: spec.Epoch, RuntimeGeneration: spec.RuntimeGeneration}
	return newTestProvider(t, engine), engine, expected, spec
}

type existingCreatedIDEngine struct{ *fakeEngine }

func (e existingCreatedIDEngine) CreateContainer(ctx context.Context, name string, request CreateContainerRequest) (CreateContainerResponse, error) {
	response, err := e.fakeEngine.CreateContainer(ctx, name, request)
	response.ID = strings.Repeat("f", 64)
	return response, err
}

func TestRuntimeIDProjectionPreservesLogicalProviderRef(t *testing.T) {
	engine := sandboxTestEngine(t)
	p := newTestProvider(t, engine)
	spec := dockerSpec()
	name := containerName(spec.SlotID)
	for _, ref := range []string{name, engine.container.ID} {
		status, err := p.Inspect(context.Background(), ref)
		if err != nil || status.ProviderRef != ref || status.RuntimeID != engine.container.ID {
			t.Fatal("inspection changed logical reference or lost physical identity")
		}
	}
	reused, err := p.Create(context.Background(), spec)
	if err != nil || reused.ProviderRef != name || reused.RuntimeID != engine.container.ID {
		t.Fatal("existing create lost the physical identity")
	}
	fresh := existingCreatedIDEngine{&fakeEngine{networkInspectError: notFound(), inspectError: notFound()}}
	created, err := newTestProvider(t, fresh).Create(context.Background(), spec)
	if err != nil || created.ProviderRef != name || created.RuntimeID != strings.Repeat("f", 64) {
		t.Fatal("new create did not preserve the returned physical identity")
	}
}

func requireOnlyExistingReads(t *testing.T, engine *existingRecordingEngine) {
	t.Helper()
	for _, call := range engine.calls {
		if call != "inspect-container" && call != "inspect-image" && call != "inspect-network" {
			t.Fatalf("non-read operation: %s", call)
		}
	}
}

func TestValidateExistingReadsOneExactCIDAndCompleteSpec(t *testing.T) {
	p, e, want, spec := existingFixture(t)
	if p.ValidateExisting(context.Background(), want, spec) != nil {
		t.Fatal("valid instance rejected")
	}
	if !reflect.DeepEqual(e.calls, []string{"inspect-container", "inspect-image", "inspect-network"}) || !reflect.DeepEqual(e.refs, []string{want.RuntimeID}) {
		t.Fatal("unexpected lookup sequence")
	}
	want.ProviderRef = want.RuntimeID
	if p.ValidateExisting(context.Background(), want, spec) != nil {
		t.Fatal("exact-CID reference rejected")
	}
	// A stopped, correctly identified runtime is a valid START candidate.
	e.container.State.Running = false
	e.container.State.Status = "exited"
	for name, endpoint := range e.container.NetworkSettings.Networks {
		endpoint.IPAddress = ""
		endpoint.Gateway = ""
		e.container.NetworkSettings.Networks[name] = endpoint
	}
	e.network.Containers = nil
	if p.ValidateExisting(context.Background(), want, spec) != nil {
		t.Fatal("stopped instance rejected")
	}
	requireOnlyExistingReads(t, e)
}

func TestValidateExistingRejectsInputBeforeEngine(t *testing.T) {
	for name, change := range map[string]func(*base.Instance, *base.SlotSpec){
		"name": func(i *base.Instance, _ *base.SlotSpec) { i.ProviderRef = "execution-slot-other" }, "short-cid": func(i *base.Instance, _ *base.SlotSpec) { i.RuntimeID = i.RuntimeID[:12] },
		"upper-cid": func(i *base.Instance, _ *base.SlotSpec) { i.RuntimeID = strings.ToUpper(i.RuntimeID) }, "empty-cid": func(i *base.Instance, _ *base.SlotSpec) { i.RuntimeID = "" },
		"wrong-ref-cid": func(i *base.Instance, _ *base.SlotSpec) { i.ProviderRef = strings.Repeat("e", 64) },
		"slot":          func(i *base.Instance, _ *base.SlotSpec) { i.SlotID = "other" }, "epoch": func(i *base.Instance, _ *base.SlotSpec) { i.Epoch++ }, "generation": func(i *base.Instance, _ *base.SlotSpec) { i.RuntimeGeneration++ },
		"empty-account": func(_ *base.Instance, s *base.SlotSpec) { s.AccountID = "" }, "invalid-image": func(_ *base.Instance, s *base.SlotSpec) { s.ImageDigest = "latest" },
		"root": func(_ *base.Instance, s *base.SlotSpec) { s.Security.RunAsUser = 0 }, "profile": func(_ *base.Instance, s *base.SlotSpec) { s.Security.SeccompProfile = "unapproved" },
		"security": func(_ *base.Instance, s *base.SlotSpec) { s.Security.NoNewPrivileges = false }, "direct-network": func(_ *base.Instance, s *base.SlotSpec) { s.Network.DenyDirectInternet = false },
	} {
		t.Run(name, func(t *testing.T) {
			p, e, want, spec := existingFixture(t)
			change(&want, &spec)
			if p.ValidateExisting(context.Background(), want, spec) != errExistingInstance || len(e.calls) != 0 {
				t.Fatal("bad input reached Engine")
			}
		})
	}
	p, e, want, spec := existingFixture(t)
	if p.ValidateExisting(nil, want, spec) != errExistingInstance || len(e.calls) != 0 {
		t.Fatal("nil context reached Engine")
	}
}

func TestValidateExistingRejectsSpecAndSandboxDriftWithoutMutation(t *testing.T) {
	for name, change := range map[string]func(*Provider, *existingRecordingEngine, *base.SlotSpec){
		"account": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.AccountID = "another-account" },
		"image": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) {
			s.ImageDigest = "sha256:" + strings.Repeat("f", 64)
		},
		"cpu": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.Resources.CPUMilli++ }, "memory": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.Resources.MemoryBytes++ },
		"pids": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.Resources.PIDs++ }, "tmpfs": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.Resources.TmpfsBytes += 2 << 20 },
		"user": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) { s.Security.RunAsUser = 1000 },
		"seccomp": func(p *Provider, _ *existingRecordingEngine, s *base.SlotSpec) {
			p.config.AllowedSeccompProfiles = append(p.config.AllowedSeccompProfiles, "custom")
			s.Security.SeccompProfile = "custom"
		},
		"apparmor": func(p *Provider, _ *existingRecordingEngine, s *base.SlotSpec) {
			p.config.AllowedAppArmorProfiles = append(p.config.AllowedAppArmorProfiles, "custom")
			s.Security.AppArmorProfile = "custom"
		},
		"proxy": func(_ *Provider, _ *existingRecordingEngine, s *base.SlotSpec) {
			s.Network.EgressProxyEndpoint = "http://host-agent.execution.internal:18081"
		},
		"network": func(_ *Provider, e *existingRecordingEngine, _ *base.SlotSpec) { e.network.Internal = false }, "mount": func(_ *Provider, e *existingRecordingEngine, _ *base.SlotSpec) {
			e.container.HostConfig.Binds = []string{"unexpected"}
		},
		"capability": func(_ *Provider, e *existingRecordingEngine, _ *base.SlotSpec) {
			e.container.HostConfig.CapAdd = []string{"SYS_ADMIN"}
		},
		"missing": func(_ *Provider, e *existingRecordingEngine, _ *base.SlotSpec) { e.inspectError = notFound() },
		"engine-error": func(_ *Provider, e *existingRecordingEngine, _ *base.SlotSpec) {
			e.inspectError = errors.New("synthetic private Engine diagnostic")
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, e, want, spec := existingFixture(t)
			if p.ValidateExisting(context.Background(), want, spec) != nil {
				t.Fatal("fixture rejected")
			}
			e.calls = nil
			change(p, e, &spec)
			if p.ValidateExisting(context.Background(), want, spec) != errExistingInstance {
				t.Fatal("drift accepted or raw error escaped")
			}
			requireOnlyExistingReads(t, e)
		})
	}
}

func TestValidateExistingRejectsReplacementCIDWithinSameInspect(t *testing.T) {
	p, e, want, spec := existingFixture(t)
	member := e.network.Containers[e.container.ID]
	delete(e.network.Containers, e.container.ID)
	e.container.ID = strings.Repeat("e", 64)
	e.network.Containers[e.container.ID] = member
	// The old helper intentionally accepts lookup names and echoes the lookup
	// argument, so its returned ProviderRef is not evidence of actual ID.
	old, err := p.existingSlot(context.Background(), want.RuntimeID, spec)
	if err != nil || old.ProviderRef != want.RuntimeID || old.RuntimeID == want.RuntimeID {
		t.Fatal("replacement fixture did not reach the specific old-helper boundary")
	}
	e.calls = nil
	e.refs = nil
	if p.ValidateExisting(context.Background(), want, spec) != errExistingInstance || !reflect.DeepEqual(e.refs, []string{want.RuntimeID}) {
		t.Fatal("replacement instance admitted")
	}
	requireOnlyExistingReads(t, e)
}

func TestValidateExistingChecksCancellationBeforeAndAfterIO(t *testing.T) {
	for _, phase := range []string{"before", "container", "image", "network", "clock"} {
		t.Run(phase, func(t *testing.T) {
			p, e, want, spec := existingFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before" {
				cancel()
			} else if phase == "clock" {
				p.config.Now = func() time.Time { cancel(); return time.Unix(2_000_000_000, 0) }
			} else {
				e.hook = func(operation string) {
					if operation == phase {
						cancel()
					}
				}
			}
			if p.ValidateExisting(ctx, want, spec) != errExistingInstance {
				t.Fatal("cancelled validation admitted")
			}
			if phase == "before" && len(e.calls) != 0 {
				t.Fatal("cancelled request reached Engine")
			}
			requireOnlyExistingReads(t, e)
		})
	}
}
