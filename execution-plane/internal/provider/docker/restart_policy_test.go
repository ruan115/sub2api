package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

// Policies Docker would act on itself. Each one would bring the container back
// with a fresh tmpfs identity and no certificate, without the host agent ever
// running bootstrap, and would stop the crash from ever surfacing as stopped.
func restartPolicyRejectedCases() map[string]RestartPolicy {
	return map[string]RestartPolicy{
		"unless-stopped":          {Name: "unless-stopped"},
		"always":                  {Name: "always"},
		"on-failure":              {Name: "on-failure"},
		"on-failure with retries": {Name: "on-failure", MaximumRetryCount: 5},
		"unknown name":            {Name: "sometimes"},
		// "no" with a retry budget is malformed evidence, not a safe default.
		"no with retries":    {Name: "no", MaximumRetryCount: 3},
		"empty with retries": {MaximumRetryCount: 1},
	}
}

// Decode through the real HTTP Engine adapter so the check is made against what
// an engine actually reports, not against a hand-built struct.
func inspectRestartPolicyJSON(t *testing.T, original Container, value string) (Container, error) {
	t.Helper()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var document, host map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document["HostConfig"], &host); err != nil {
		t.Fatal(err)
	}
	delete(host, "RestartPolicy")
	if value != "" {
		host["RestartPolicy"] = json.RawMessage(value)
	}
	document["HostConfig"], err = json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.43/containers/"+original.ID+"/json" || r.URL.RawQuery != "" {
			t.Error("restart policy evidence did not use an exact read-only container inspect")
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, apiPrefix: "/v1.43"}
	container, inspectErr := engine.InspectContainer(context.Background(), original.ID)
	if reads.Load() != 1 {
		t.Fatal("inspect request not made exactly once")
	}
	return container, inspectErr
}

func TestRestartPolicyCreateRequestDisablesDockerRestarts(t *testing.T) {
	engine := &fakeEngine{networkInspectError: notFound(), inspectError: notFound()}
	p := sandboxProviderWithBootstrap(t, engine)
	if _, err := p.Create(context.Background(), dockerSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	policy := engine.createRequest.HostConfig.RestartPolicy
	if policy.Name != "no" || policy.MaximumRetryCount != 0 {
		t.Fatalf("create requested restart policy %+v, want an explicit disabled policy", policy)
	}

	// The request must also serialize as an explicit "no" on the wire; an
	// omitted policy would let a future engine default decide instead.
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/containers/create") {
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
		var document struct {
			HostConfig map[string]json.RawMessage `json:"HostConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&document); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		raw, exists := document.HostConfig["RestartPolicy"]
		if !exists {
			t.Error("create request omitted RestartPolicy")
		} else {
			var wire RestartPolicy
			if err := json.Unmarshal(raw, &wire); err != nil || wire.Name != "no" || wire.MaximumRetryCount != 0 {
				t.Errorf("create request serialized restart policy %s", raw)
			}
		}
		creates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"` + engine.container.ID + `"}`))
	}))
	defer server.Close()
	httpEngine := &HTTPEngine{client: server.Client(), baseURL: server.URL, apiPrefix: "/v1.43"}
	if _, err := httpEngine.CreateContainer(context.Background(), engine.createdName, engine.createRequest); err != nil || creates.Load() != 1 {
		t.Fatalf("typed restart policy create serialization: %v", err)
	}
}

func TestRestartPolicyReadbackRejectsAtEveryAdoptionEntrypoint(t *testing.T) {
	for name, policy := range restartPolicyRejectedCases() {
		t.Run(name, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			p := sandboxProviderWithBootstrap(t, engine)
			// Prove the fixture is acceptable before mutating this one field.
			positive, err := inspectRestartPolicyJSON(t, engine.container, `{"Name":"no","MaximumRetryCount":0}`)
			if err != nil || !restartPolicyIsDisabled(positive.HostConfig.RestartPolicy) {
				t.Fatalf("positive HTTP readback failed: %v", err)
			}
			engine.container = positive
			assertSandboxAcceptsAllEntrypoints(t, p, engine)
			spec := dockerSpec()
			expected := base.Instance{
				ProviderRef: containerName(spec.SlotID), RuntimeID: engine.container.ID,
				SlotID: spec.SlotID, Epoch: spec.Epoch, RuntimeGeneration: spec.RuntimeGeneration,
			}
			if err := p.ValidateExisting(context.Background(), expected, spec); err != nil {
				t.Fatalf("positive existing-instance readback failed: %v", err)
			}

			engine.calls = nil
			encoded, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			unsafe, err := inspectRestartPolicyJSON(t, engine.container, string(encoded))
			if err != nil {
				t.Fatalf("valid JSON evidence could not be decoded: %v", err)
			}
			engine.container = unsafe
			assertSandboxRejectsAllEntrypoints(t, p, engine)
			if err := p.ValidateExisting(context.Background(), expected, spec); !errors.Is(err, errExistingInstance) {
				t.Fatalf("strict existing-instance startup accepted an auto-restarting container: %v", err)
			}
			assertSwapNoWrites(t, engine)
			if engine.container.HostConfig.RestartPolicy != unsafe.HostConfig.RestartPolicy {
				t.Fatal("auto-restarting container was silently repaired")
			}
		})
	}
}

// Only an explicit "no" is evidence. Absent, null and an empty name all decode
// to a zero value that carries no proof the engine disabled restarts, and are
// refused for the same reason a missing MemorySwap is.
func TestRestartPolicyRequiresPositiveEvidence(t *testing.T) {
	for name, value := range map[string]string{
		"absent":       "",
		"null":         "null",
		"empty object": `{}`,
		"empty name":   `{"Name":"","MaximumRetryCount":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			p := sandboxProviderWithBootstrap(t, engine)
			assertSandboxAcceptsAllEntrypoints(t, p, engine)
			engine.calls = nil
			absent, err := inspectRestartPolicyJSON(t, engine.container, value)
			if err != nil {
				t.Fatalf("readback: %v", err)
			}
			if restartPolicyIsDisabled(absent.HostConfig.RestartPolicy) {
				t.Fatalf("policy %+v was accepted as positive evidence", absent.HostConfig.RestartPolicy)
			}
			engine.container = absent
			assertSandboxRejectsAllEntrypoints(t, p, engine)
			assertSwapNoWrites(t, engine)
		})
	}
}

// Coverage parity with the no-swap rule: bootstrap re-checks the sandbox both
// before and after the exec, so drift appearing at either point must be caught
// rather than producing a retryable-looking success.
func TestRestartPolicyBootstrapPreAndPostExecRejectDrift(t *testing.T) {
	for name, policy := range restartPolicyRejectedCases() {
		for _, operation := range []string{"request", "install"} {
			for _, afterExec := range []bool{false, true} {
				stage := "before"
				if afterExec {
					stage = "after"
				}
				t.Run(name+"/"+operation+"/"+stage, func(t *testing.T) {
					p, engine, instance, spec, bundle := bootstrapProviderFixture(t)
					invoke := func() error {
						if operation == "install" {
							return p.BootstrapInstall(context.Background(), instance, spec, bundle)
						}
						_, err := p.BootstrapRequest(context.Background(), instance, spec)
						return err
					}
					if err := invoke(); err != nil || engine.execs != 1 {
						t.Fatalf("positive bootstrap failed: %v", err)
					}
					engine.execs, engine.calls = 0, nil
					encoded, err := json.Marshal(policy)
					if err != nil {
						t.Fatal(err)
					}
					unsafe, err := inspectRestartPolicyJSON(t, engine.container, string(encoded))
					if err != nil {
						t.Fatal(err)
					}
					if afterExec {
						engine.afterExec = func() { engine.container = unsafe }
					} else {
						engine.container = unsafe
					}
					if err := invoke(); !errors.Is(err, runtimebootstrap.ErrBootstrap) || errors.Is(err, runtimebootstrap.ErrNotReady) {
						t.Fatalf("unsafe bootstrap accepted or marked retryable: %v", err)
					}
					wantExecs := 0
					if afterExec {
						wantExecs = 1
					}
					if engine.execs != wantExecs {
						t.Fatalf("unexpected exec count: %d", engine.execs)
					}
					assertSwapNoWrites(t, engine.fakeEngine)
					if engine.container.HostConfig.RestartPolicy != unsafe.HostConfig.RestartPolicy {
						t.Fatal("bootstrap repaired the drifted restart policy")
					}
				})
			}
		}
	}
}
