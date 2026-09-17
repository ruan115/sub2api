package docker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var bootstrapHTTPCID = strings.Repeat("c", 64)
var bootstrapHTTPExecID = strings.Repeat("e", 64)

type bootstrapHTTPScenario struct {
	command       []string
	mediaType     string
	raw           []byte
	createStatus  int
	startStatus   int
	inspectStatus int
	inspectBody   string
	hold          <-chan struct{}
}

type bootstrapHTTPPeer struct {
	mu      sync.Mutex
	calls   []string
	started chan struct{}
}

func (p *bootstrapHTTPPeer) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *bootstrapHTTPPeer) requireCalls(t *testing.T, calls ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(p.calls, calls) {
		t.Fatalf("unexpected Engine operation order: got %v, want %v", p.calls, calls)
	}
}

// Use the real Unix-socket HTTPEngine and a hijacked, close-delimited HTTP 200
// response. Ordinary httptest ResponseWriter.Write would hide this protocol
// boundary behind Content-Length or chunked transfer encoding. This is an
// Engine wire regression, not evidence of a real Docker daemon or sandbox.
func newBootstrapHTTPPeer(t *testing.T, scenario bootstrapHTTPScenario) (*HTTPEngine, *bootstrapHTTPPeer) {
	t.Helper()
	peer := &bootstrapHTTPPeer{started: make(chan struct{})}
	if scenario.mediaType == "" {
		scenario.mediaType = "application/vnd.docker.multiplexed-stream"
	}
	if scenario.inspectBody == "" {
		scenario.inspectBody = bootstrapHTTPInspect(false, 0, bootstrapHTTPCID, bootstrapHTTPExecID)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_ping":
			if r.Method != http.MethodHead {
				t.Error("expected HEAD ping")
			}
			w.WriteHeader(http.StatusOK)
			return
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ApiVersion":"1.43","MinAPIVersion":"1.41"}`)
			return
		case "/v1.43/containers/" + bootstrapHTTPCID + "/exec":
			peer.record("create")
			if scenario.createStatus != 0 {
				w.WriteHeader(scenario.createStatus)
				_, _ = io.WriteString(w, `{"message":"Authorization: must-not-escape"}`)
				return
			}
			var request struct {
				AttachStdout bool
				AttachStderr bool
				User         string
				Cmd          []string
			}
			decoder := json.NewDecoder(io.LimitReader(r.Body, runtimebootstrap.MaxEncodedBundleBytes+1024))
			decoder.DisallowUnknownFields()
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				decoder.Decode(&request) != nil || !request.AttachStdout || !request.AttachStderr ||
				request.User != "1000:1000" || !slices.Equal(request.Cmd, scenario.command) {
				t.Error("unsafe or incorrectly encoded bootstrap exec-create request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"Id":%q}`, bootstrapHTTPExecID)
		case "/v1.43/exec/" + bootstrapHTTPExecID + "/start":
			peer.record("start")
			body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("Upgrade") != "" || err != nil || string(body) != `{"Detach":false,"Tty":false}` {
				t.Error("exec start must request an attached, non-TTY JSON operation without protocol upgrade")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if scenario.startStatus != 0 {
				w.WriteHeader(scenario.startStatus)
				_, _ = io.WriteString(w, `{"message":"Authorization: must-not-escape"}`)
				return
			}
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error("hijack exec-start response failed")
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(buffer, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\n\r\n", scenario.mediaType)
			if buffer.Flush() != nil {
				return
			}
			close(peer.started)
			if scenario.hold != nil {
				<-scenario.hold
				return
			}
			// Split both frame headers and payload across socket writes. There is
			// deliberately no HTTP length, chunk wrapper, or final JSON object.
			for remaining := scenario.raw; len(remaining) > 0; {
				n := min(len(remaining), 31)
				if _, err := conn.Write(remaining[:n]); err != nil {
					return
				}
				remaining = remaining[n:]
			}
		case "/v1.43/exec/" + bootstrapHTTPExecID + "/json":
			peer.record("inspect")
			if r.Method != http.MethodGet {
				t.Error("expected GET exec inspect")
			}
			w.Header().Set("Content-Type", "application/json")
			if scenario.inspectStatus != 0 {
				w.WriteHeader(scenario.inspectStatus)
			}
			_, _ = io.WriteString(w, scenario.inspectBody)
		default:
			t.Error("bootstrap contacted an unexpected Engine endpoint")
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// A short path also works under Darwin's tighter Unix sockaddr path limit.
	directory, err := os.MkdirTemp("", "bootstrap-http-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "engine.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	engine, err := NewHTTPEngine(HTTPConfig{SocketPath: socket, UserAgent: "bootstrap-http-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.client.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return engine, peer
}

func bootstrapHTTPInspect(running bool, exit int, cid, execID string) string {
	return fmt.Sprintf(`{"ID":%q,"ContainerID":%q,"Running":%t,"ExitCode":%d}`, execID, cid, running, exit)
}

func bootstrapHTTPPublicMaterial(t *testing.T) ([]byte, runtimebootstrap.PublicBundle) {
	t.Helper()
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-http", NodeID: "node-http", Epoch: 7, Generation: 9}
	uri, err := binding.URI()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{uri}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	request, err := json.Marshal(runtimebootstrap.Request{Binding: binding, CSRPEM: csr})
	if err != nil {
		t.Fatal(err)
	}
	ca, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueRuntime(binding, csr)
	if err != nil {
		t.Fatal(err)
	}
	return append(request, '\n'), runtimebootstrap.PublicBundle{CertificatePEM: leaf.CertificatePEM, CAPEM: ca.CertificatePEM()}
}

func TestBootstrapHTTPUnixHijackRequestAndInstall(t *testing.T) {
	request, bundle := bootstrapHTTPPublicMaterial(t)
	argument, err := runtimebootstrap.EncodeBundleArgument(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, mediaType := range []string{"application/vnd.docker.raw-stream", "application/vnd.docker.multiplexed-stream"} {
		t.Run(strings.TrimPrefix(mediaType, "application/vnd.docker."), func(t *testing.T) {
			t.Run("request", func(t *testing.T) {
				raw := append(dockerStreamFrame(1, request[:17]), dockerStreamFrame(1, request[17:])...)
				engine, peer := newBootstrapHTTPPeer(t, bootstrapHTTPScenario{command: []string{"/worker", "bootstrap-request"}, mediaType: mediaType, raw: raw})
				got, err := engine.BootstrapRequestExec(context.Background(), bootstrapHTTPCID, 1000)
				if err != nil || !bytes.Equal(got, request) {
					t.Fatal("public CSR request did not survive real HTTP framing", err)
				}
				if _, err := runtimebootstrap.DecodeRequest(got); err != nil {
					t.Fatal("returned request is not a valid canonical CSR envelope")
				}
				peer.requireCalls(t, "create", "start", "inspect")
			})
			t.Run("install", func(t *testing.T) {
				raw := append(dockerStreamFrame(1, []byte("bootstrap-")), dockerStreamFrame(1, []byte("installed\n"))...)
				engine, peer := newBootstrapHTTPPeer(t, bootstrapHTTPScenario{command: []string{"/worker", "bootstrap-install", argument}, mediaType: mediaType, raw: raw})
				if err := engine.BootstrapInstallExec(context.Background(), bootstrapHTTPCID, 1000, bundle); err != nil {
					t.Fatal("public certificate install did not survive real HTTP framing", err)
				}
				peer.requireCalls(t, "create", "start", "inspect")
			})
		})
	}
}

func TestBootstrapHTTPRejectsWireAndExecFailures(t *testing.T) {
	good := dockerStreamFrame(1, []byte("public CSR"))
	retry := dockerStreamFrame(2, []byte("runtime bootstrap rejected\n"))
	for _, tc := range []struct {
		name     string
		scenario bootstrapHTTPScenario
		want     error
		calls    []string
	}{
		{"create error", bootstrapHTTPScenario{createStatus: 500}, runtimebootstrap.ErrBootstrap, []string{"create"}},
		{"start error", bootstrapHTTPScenario{startStatus: 409}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"empty attached stream", bootstrapHTTPScenario{}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"JSON is not a stream", bootstrapHTTPScenario{raw: []byte(`{"message":"private-value"}`)}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"truncated frame", bootstrapHTTPScenario{raw: good[:len(good)-1]}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"daemon system error", bootstrapHTTPScenario{raw: dockerStreamFrame(3, []byte("private-value"))}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"stdout limit", bootstrapHTTPScenario{raw: dockerStreamFrame(1, bytes.Repeat([]byte("x"), runtimebootstrap.MaxPublicBytes+1))}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"Engine response limit", bootstrapHTTPScenario{raw: dockerStreamFrame(1, bytes.Repeat([]byte("x"), maxEngineResponseBytes+1))}, runtimebootstrap.ErrBootstrap, []string{"create", "start"}},
		{"inspect error", bootstrapHTTPScenario{raw: good, inspectStatus: 500}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"still running", bootstrapHTTPScenario{raw: good, inspectBody: bootstrapHTTPInspect(true, 0, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"wrong container", bootstrapHTTPScenario{raw: good, inspectBody: bootstrapHTTPInspect(false, 0, strings.Repeat("d", 64), bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"wrong exec", bootstrapHTTPScenario{raw: good, inspectBody: bootstrapHTTPInspect(false, 0, bootstrapHTTPCID, strings.Repeat("f", 64))}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"missing exit", bootstrapHTTPScenario{raw: good, inspectBody: `{"Running":false}`}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"nonzero exit", bootstrapHTTPScenario{raw: good, inspectBody: bootstrapHTTPInspect(false, 2, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"request rejection remains generic", bootstrapHTTPScenario{raw: retry, inspectBody: bootstrapHTTPInspect(false, 2, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"retry exactly 75", bootstrapHTTPScenario{raw: retry, inspectBody: bootstrapHTTPInspect(false, 75, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrNotReady, []string{"create", "start", "inspect"}},
		{"75 with stdout", bootstrapHTTPScenario{raw: append(append([]byte(nil), good...), retry...), inspectBody: bootstrapHTTPInspect(false, 75, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
		{"75 with unknown stderr", bootstrapHTTPScenario{raw: dockerStreamFrame(2, []byte("private-value")), inspectBody: bootstrapHTTPInspect(false, 75, bootstrapHTTPCID, bootstrapHTTPExecID)}, runtimebootstrap.ErrBootstrap, []string{"create", "start", "inspect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.scenario.command = []string{"/worker", "bootstrap-request"}
			engine, peer := newBootstrapHTTPPeer(t, tc.scenario)
			got, err := engine.BootstrapRequestExec(context.Background(), bootstrapHTTPCID, 1000)
			if err != tc.want || len(got) != 0 {
				t.Fatal("wire or exec failure was not normalized", err)
			}
			peer.requireCalls(t, tc.calls...)
		})
	}
}

func TestBootstrapHTTPCancelClosesAttachedRead(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	engine, peer := newBootstrapHTTPPeer(t, bootstrapHTTPScenario{command: []string{"/worker", "bootstrap-request"}, hold: hold})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := engine.BootstrapRequestExec(ctx, bootstrapHTTPCID, 1000)
		done <- err
	}()
	select {
	case <-peer.started:
	case <-time.After(time.Second):
		t.Fatal("exec-start was not attached")
	}
	cancel()
	select {
	case err := <-done:
		if err != runtimebootstrap.ErrBootstrap {
			t.Fatal("cancelled read did not fail closed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt close-delimited response read")
	}
	peer.requireCalls(t, "create", "start")
}

func TestBootstrapHTTPInstallAcknowledgmentAndPublicBundleLimit(t *testing.T) {
	_, bundle := bootstrapHTTPPublicMaterial(t)
	// Harmless trailing PEM whitespace produces a valid public-only argument
	// above the old general-purpose ExecContainer's 4 KiB argument limit.
	large := runtimebootstrap.PublicBundle{
		CertificatePEM: append(bytes.Clone(bundle.CertificatePEM), bytes.Repeat([]byte(" "), 6*1024)...),
		CAPEM:          append(bytes.Clone(bundle.CAPEM), bytes.Repeat([]byte(" "), 6*1024)...),
	}
	argument, err := runtimebootstrap.EncodeBundleArgument(large)
	if err != nil || len(argument) <= 4096 || len(argument) > runtimebootstrap.MaxEncodedBundleBytes {
		t.Fatal("large public bundle fixture is not within the fixed command envelope")
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		exit int
		want error
	}{
		{"large public argument", dockerStreamFrame(1, []byte("bootstrap-installed\n")), 0, nil},
		{"wrong acknowledgment", dockerStreamFrame(1, []byte("ok\n")), 0, runtimebootstrap.ErrBootstrap},
		{"stderr on success", append(dockerStreamFrame(1, []byte("bootstrap-installed\n")), dockerStreamFrame(2, []byte("private-value"))...), 0, runtimebootstrap.ErrBootstrap},
		{"install lock retry", dockerStreamFrame(2, []byte("runtime bootstrap rejected\n")), 75, runtimebootstrap.ErrNotReady},
		{"explicit install rejection", dockerStreamFrame(2, []byte("runtime bootstrap rejected\n")), 2, runtimebootstrap.ErrInstallRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, peer := newBootstrapHTTPPeer(t, bootstrapHTTPScenario{
				command:     []string{"/worker", "bootstrap-install", argument},
				raw:         tc.raw,
				inspectBody: bootstrapHTTPInspect(false, tc.exit, bootstrapHTTPCID, bootstrapHTTPExecID),
			})
			if err := engine.BootstrapInstallExec(context.Background(), bootstrapHTTPCID, 1000, large); err != tc.want {
				t.Fatal("unexpected install acknowledgment result", err)
			}
			peer.requireCalls(t, "create", "start", "inspect")
		})
	}
	t.Run("oversize is rejected before exec create", func(t *testing.T) {
		oversize := runtimebootstrap.PublicBundle{
			CertificatePEM: append(bytes.Clone(bundle.CertificatePEM), bytes.Repeat([]byte(" "), 14*1024-len(bundle.CertificatePEM))...),
			CAPEM:          append(bytes.Clone(bundle.CAPEM), bytes.Repeat([]byte(" "), 14*1024-len(bundle.CAPEM))...),
		}
		engine, peer := newBootstrapHTTPPeer(t, bootstrapHTTPScenario{})
		if err := engine.BootstrapInstallExec(context.Background(), bootstrapHTTPCID, 1000, oversize); err != runtimebootstrap.ErrBootstrap {
			t.Fatal("oversize public bundle accepted")
		}
		peer.requireCalls(t)
	})
}

func TestBootstrapHTTPInstallRejectionDoesNotMaskTransportOrIdentityFailure(t *testing.T) {
	_, bundle := bootstrapHTTPPublicMaterial(t)
	argument, err := runtimebootstrap.EncodeBundleArgument(bundle)
	if err != nil {
		t.Fatal(err)
	}
	rejected := dockerStreamFrame(2, []byte("runtime bootstrap rejected\n"))
	for _, tc := range []struct {
		name     string
		scenario bootstrapHTTPScenario
		calls    []string
	}{
		{"create error", bootstrapHTTPScenario{createStatus: 500}, []string{"create"}},
		{"start error", bootstrapHTTPScenario{startStatus: 409}, []string{"create", "start"}},
		{"inspect error", bootstrapHTTPScenario{raw: rejected, inspectStatus: 500}, []string{"create", "start", "inspect"}},
		{"closed stream", bootstrapHTTPScenario{}, []string{"create", "start"}},
		{"truncated frame", bootstrapHTTPScenario{raw: rejected[:len(rejected)-1]}, []string{"create", "start"}},
		{"wrong stderr", bootstrapHTTPScenario{raw: dockerStreamFrame(2, []byte("private-value"))}, []string{"create", "start", "inspect"}},
		{"stderr missing newline", bootstrapHTTPScenario{raw: dockerStreamFrame(2, []byte("runtime bootstrap rejected"))}, []string{"create", "start", "inspect"}},
		{"stdout present", bootstrapHTTPScenario{raw: append(dockerStreamFrame(1, []byte("unexpected")), rejected...)}, []string{"create", "start", "inspect"}},
		{"wrong exit", bootstrapHTTPScenario{raw: rejected, inspectBody: bootstrapHTTPInspect(false, 1, bootstrapHTTPCID, bootstrapHTTPExecID)}, []string{"create", "start", "inspect"}},
		{"zero exit", bootstrapHTTPScenario{raw: rejected, inspectBody: bootstrapHTTPInspect(false, 0, bootstrapHTTPCID, bootstrapHTTPExecID)}, []string{"create", "start", "inspect"}},
		{"wrong CID", bootstrapHTTPScenario{raw: rejected, inspectBody: bootstrapHTTPInspect(false, 2, strings.Repeat("d", 64), bootstrapHTTPExecID)}, []string{"create", "start", "inspect"}},
		{"wrong exec", bootstrapHTTPScenario{raw: rejected, inspectBody: bootstrapHTTPInspect(false, 2, bootstrapHTTPCID, strings.Repeat("f", 64))}, []string{"create", "start", "inspect"}},
		{"still running", bootstrapHTTPScenario{raw: rejected, inspectBody: bootstrapHTTPInspect(true, 2, bootstrapHTTPCID, bootstrapHTTPExecID)}, []string{"create", "start", "inspect"}},
		{"missing exit", bootstrapHTTPScenario{raw: rejected, inspectBody: `{"Running":false}`}, []string{"create", "start", "inspect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.scenario.command = []string{"/worker", "bootstrap-install", argument}
			if tc.scenario.inspectBody == "" {
				tc.scenario.inspectBody = bootstrapHTTPInspect(false, 2, bootstrapHTTPCID, bootstrapHTTPExecID)
			}
			engine, peer := newBootstrapHTTPPeer(t, tc.scenario)
			if err := engine.BootstrapInstallExec(context.Background(), bootstrapHTTPCID, 1000, bundle); err != runtimebootstrap.ErrBootstrap {
				t.Fatal("transport or identity failure was accepted as explicit worker refusal", err)
			}
			peer.requireCalls(t, tc.calls...)
		})
	}
}
