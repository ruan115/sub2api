package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

func swapJSONCases(memory int64) map[string]string {
	return map[string]string{
		"missing": "", "null": "null", "zero": "0", "unlimited": "-1", "negative": "-2",
		"less than memory": strconv.FormatInt(memory-1, 10),
		"more than memory": strconv.FormatInt(memory+1, 10),
	}
}

// Decode through the actual HTTP Engine adapter. Both omitted and explicit null
// must remain absent evidence, not inherit a safe value from a fixture/default.
func inspectSwapJSON(t *testing.T, original Container, value string) (Container, error) {
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
	delete(host, "MemorySwap")
	if value != "" {
		host["MemorySwap"] = json.RawMessage(value)
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
			t.Error("swap evidence did not use an exact read-only container inspect")
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

func TestMemorySwapCreateIsExplicitInProviderAndHTTPJSON(t *testing.T) {
	engine := &fakeEngine{networkInspectError: notFound(), inspectError: notFound()}
	p := newTestProvider(t, engine)
	spec := dockerSpec()
	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	request := engine.createRequest
	if request.HostConfig.Memory <= 0 || request.HostConfig.Memory != spec.Resources.MemoryBytes || request.HostConfig.MemorySwap != request.HostConfig.Memory {
		t.Fatal("provider did not request explicit no-swap policy")
	}
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.43/containers/create" || r.URL.Query().Get("name") != engine.createdName {
			t.Error("incorrect container create request")
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
		var document struct{ HostConfig map[string]json.RawMessage }
		if json.NewDecoder(r.Body).Decode(&document) != nil {
			t.Error("create request was not JSON")
		}
		expected := strconv.FormatInt(spec.Resources.MemoryBytes, 10)
		if string(document.HostConfig["Memory"]) != expected || string(document.HostConfig["MemorySwap"]) != expected {
			t.Error("actual HTTP JSON omitted or changed no-swap limits")
		}
		creates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"container-created","Warnings":[]}`)
	}))
	defer server.Close()
	httpEngine := &HTTPEngine{client: server.Client(), baseURL: server.URL, apiPrefix: "/v1.43"}
	if _, err := httpEngine.CreateContainer(context.Background(), engine.createdName, request); err != nil || creates.Load() != 1 {
		t.Fatalf("typed no-swap create serialization: %v", err)
	}
}

func TestMemorySwapReadbackRejectsUnsafeEvidenceAtEveryAdoptionEntrypoint(t *testing.T) {
	for name, value := range swapJSONCases(dockerSpec().Resources.MemoryBytes) {
		t.Run(name, func(t *testing.T) {
			engine := sandboxTestEngine(t)
			p := sandboxProviderWithBootstrap(t, engine)
			// Prove this exact fixture is acceptable before mutating one field.
			positive, err := inspectSwapJSON(t, engine.container, strconv.FormatInt(engine.container.HostConfig.Memory, 10))
			if err != nil || positive.HostConfig.MemorySwap != positive.HostConfig.Memory {
				t.Fatalf("positive HTTP readback failed: %v", err)
			}
			engine.container = positive
			assertSandboxAcceptsAllEntrypoints(t, p, engine)
			spec := dockerSpec()
			expected := base.Instance{ProviderRef: containerName(spec.SlotID), RuntimeID: engine.container.ID, SlotID: spec.SlotID, Epoch: spec.Epoch, RuntimeGeneration: spec.RuntimeGeneration}
			if err := p.ValidateExisting(context.Background(), expected, spec); err != nil {
				t.Fatalf("positive existing-instance readback failed: %v", err)
			}
			engine.calls = nil
			unsafe, err := inspectSwapJSON(t, engine.container, value)
			if err != nil {
				t.Fatalf("valid JSON evidence could not be decoded: %v", err)
			}
			engine.container = unsafe
			assertSandboxRejectsAllEntrypoints(t, p, engine)
			if err := p.ValidateExisting(context.Background(), expected, spec); !errors.Is(err, errExistingInstance) {
				t.Fatalf("strict existing-instance startup accepted unsafe swap evidence: %v", err)
			}
			assertSwapNoWrites(t, engine)
			if engine.container.HostConfig.MemorySwap != unsafe.HostConfig.MemorySwap || engine.container.HostConfig.Memory != unsafe.HostConfig.Memory {
				t.Fatal("unsafe old container was silently repaired")
			}
		})
	}
}

func TestMemorySwapHTTPDecoderRejectsWrongTypesAndOverflow(t *testing.T) {
	original, _ := sandboxFixture(t)
	for _, value := range []string{`"536870912"`, "true", "536870912.5", "9223372036854775808"} {
		if _, err := inspectSwapJSON(t, original, value); err == nil {
			t.Fatal("invalid swap evidence type was accepted")
		}
	}
}

func TestMemorySwapRequiresPositiveMemoryEvenWhenEqual(t *testing.T) {
	for _, memory := range []int64{0, -1} {
		engine := sandboxTestEngine(t)
		p := sandboxProviderWithBootstrap(t, engine)
		assertSandboxAcceptsAllEntrypoints(t, p, engine)
		engine.container.HostConfig.Memory, engine.container.HostConfig.MemorySwap = memory, memory
		assertSandboxRejectsAllEntrypoints(t, p, engine)
		assertSwapNoWrites(t, engine)
	}
}

func TestMemorySwapBootstrapPreAndPostExecRejectDrift(t *testing.T) {
	for name, value := range swapJSONCases(dockerSpec().Resources.MemoryBytes) {
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
					unsafe, err := inspectSwapJSON(t, engine.container, value)
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
					if engine.container.HostConfig.MemorySwap != unsafe.HostConfig.MemorySwap {
						t.Fatal("bootstrap repaired drifted swap policy")
					}
				})
			}
		}
	}
}

func assertSwapNoWrites(t *testing.T, engine *fakeEngine) {
	t.Helper()
	for _, call := range engine.calls {
		if call != "ping" && !strings.HasPrefix(call, "inspect-") {
			t.Fatalf("swap rejection mutated Docker state through %s", call)
		}
	}
}
