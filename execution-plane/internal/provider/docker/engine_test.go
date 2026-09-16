package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPEngineExecContainer(t *testing.T) {
	var createRequest CreateExecRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1.43/containers/worker-1/exec":
			if request.Method != http.MethodPost {
				t.Fatalf("create exec method = %s", request.Method)
			}
			if err := json.NewDecoder(request.Body).Decode(&createRequest); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, `{"Id":"exec-1"}`)
		case "/v1.43/exec/exec-1/start":
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			_, _ = w.Write(dockerStreamFrame(1, []byte("stdout\n")))
			_, _ = w.Write(dockerStreamFrame(2, []byte("stderr\n")))
		case "/v1.43/exec/exec-1/json":
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":7}`)
		default:
			t.Fatalf("unexpected exec path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, userAgent: "test", apiPrefix: "/v1.43"}
	result, err := engine.ExecContainer(context.Background(), "worker-1", []string{"/networkprobe", "192.0.2.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || string(result.Stdout) != "stdout\n" || string(result.Stderr) != "stderr\n" {
		t.Fatalf("unexpected exec result: %+v", result)
	}
	if !createRequest.AttachStdout || !createRequest.AttachStderr || createRequest.User != "65532:65532" ||
		len(createRequest.Cmd) != 2 || createRequest.Cmd[0] != "/networkprobe" {
		t.Fatalf("unexpected create exec request: %+v", createRequest)
	}
}

func TestDecodeDockerStreamRejectsMalformedFrames(t *testing.T) {
	for _, payload := range [][]byte{
		{1},
		dockerStreamFrame(3, []byte("invalid-stream")),
		append(dockerStreamFrame(1, []byte("payload"))[:8], []byte("short")...),
	} {
		if _, _, err := decodeDockerStream(payload); err == nil {
			t.Fatalf("decodeDockerStream(%x) succeeded", payload)
		}
	}
}

func dockerStreamFrame(stream byte, payload []byte) []byte {
	var buffer bytes.Buffer
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = buffer.Write(header)
	_, _ = buffer.Write(payload)
	return buffer.Bytes()
}

func TestHTTPEngineCreateUsesTypedJSONAndQuery(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1.43/containers/create" || request.URL.Query().Get("name") != "execution-slot-1" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
		body, _ := io.ReadAll(request.Body)
		receivedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"container-1","Warnings":[]}`)
	}))
	defer server.Close()

	engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, userAgent: "test", apiPrefix: "/v1.43"}
	response, err := engine.CreateContainer(context.Background(), "execution-slot-1", CreateContainerRequest{
		Image: "registry/worker@sha256:digest",
		HostConfig: HostConfig{
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "container-1" || !strings.Contains(receivedBody, `"ReadonlyRootfs":true`) || !strings.Contains(receivedBody, `"CapDrop":["ALL"]`) {
		t.Fatalf("unexpected response/body: response=%+v body=%s", response, receivedBody)
	}
}

func TestHTTPEngineMapsErrorsAndCapsResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1.43/containers/missing/json" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container"}`)
			return
		}
		_, _ = io.WriteString(w, strings.Repeat("x", maxEngineResponseBytes+1))
	}))
	defer server.Close()
	engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, userAgent: "test", apiPrefix: "/v1.43"}

	if _, err := engine.InspectContainer(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if _, err := engine.InspectNetwork(context.Background(), "large"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected response limit error, got %v", err)
	}
	if !IsConflict(&APIError{StatusCode: http.StatusConflict}) {
		t.Fatal("expected Docker conflict classification")
	}
	if !IsNotModified(&APIError{StatusCode: http.StatusNotModified}) {
		t.Fatal("expected Docker not-modified classification")
	}
}

func TestHTTPEngineNegotiatesServerVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_ping":
			w.WriteHeader(http.StatusOK)
		case "/version":
			_, _ = io.WriteString(w, `{"ApiVersion":"1.55","MinAPIVersion":"1.40"}`)
		case "/v1.55/networks/execution-net-1":
			_, _ = io.WriteString(w, `{"Id":"network-1","Name":"execution-net-1","Internal":true}`)
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, userAgent: "test"}
	if err := engine.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	network, err := engine.InspectNetwork(context.Background(), "execution-net-1")
	if err != nil || !network.Internal {
		t.Fatalf("versioned request failed: network=%+v err=%v", network, err)
	}
}

func TestHTTPEngineRejectsOldOrMalformedVersion(t *testing.T) {
	for _, apiVersion := range []string{"1.40", "invalid"} {
		t.Run(apiVersion, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/version" {
					_, _ = io.WriteString(w, `{"ApiVersion":"`+apiVersion+`","MinAPIVersion":"1.12"}`)
				}
			}))
			defer server.Close()
			engine := &HTTPEngine{client: server.Client(), baseURL: server.URL, userAgent: "test"}
			if err := engine.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "older than required") {
				t.Fatalf("expected version rejection, got %v", err)
			}
		})
	}
}

type sandboxRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sandboxRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPEngineInspectImageIsReadOnlyAndUsesReference(t *testing.T) {
	reference := dockerSpec().ImageDigest
	want := sandboxImageFixture()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	engine := &HTTPEngine{baseURL: "http://docker", apiPrefix: "/v1.43", client: &http.Client{Transport: sandboxRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1.43/images/"+reference+"/json" || r.Body != nil || r.URL.RawQuery != "" {
			t.Fatal("image verification did not use a single read-only inspect of the immutable reference")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded))}, nil
	})}}
	image, err := engine.InspectImage(context.Background(), reference)
	if err != nil || image.ID != want.ID || len(image.RepoDigests) != 1 || image.RepoDigests[0] != reference || calls != 1 {
		t.Fatalf("image inspect: %v", err)
	}
}

func TestHTTPEngineDecodesActualSandboxSecurityFields(t *testing.T) {
	raw := `{"Image":"sha256:actual","Config":{"Image":"repo@sha256:manifest","Hostname":"sandbox"},"HostConfig":{"Privileged":true,"CapAdd":["NET_ADMIN"],"PidMode":"host","IpcMode":"host","UTSMode":"host","UsernsMode":"host","CgroupnsMode":"host","Runtime":"custom","Mounts":[{"Type":"bind"}],"Devices":[{"PathOnHost":"/dev/kvm"}],"DeviceRequests":[{"Count":-1}],"DeviceCgroupRules":["a *:* rwm"],"VolumesFrom":["other"],"PublishAllPorts":true},"NetworkSettings":{"Networks":{"outside":{"NetworkID":"network","IPAddress":"172.31.0.2","Gateway":"172.31.0.1","GlobalIPv6Address":"fd00::2"}}},"Mounts":[{"Type":"tmpfs","Source":"","Destination":"/tmp","RW":true}]}`
	engine := &HTTPEngine{baseURL: "http://docker", apiPrefix: "/v1.43", client: &http.Client{Transport: sandboxRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("inspect mutated state")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}}
	c, err := engine.InspectContainer(context.Background(), "container")
	if err != nil || !c.HostConfig.Privileged || len(c.HostConfig.CapAdd) != 1 || c.HostConfig.PidMode != "host" || c.HostConfig.IpcMode != "host" ||
		c.HostConfig.UTSMode != "host" || c.HostConfig.UsernsMode != "host" || c.HostConfig.CgroupnsMode != "host" || c.HostConfig.Runtime != "custom" ||
		len(c.HostConfig.Mounts) != 1 || len(c.HostConfig.Devices) != 1 || len(c.HostConfig.DeviceRequests) != 1 || len(c.HostConfig.DeviceCgroupRules) != 1 ||
		len(c.HostConfig.VolumesFrom) != 1 || !c.HostConfig.PublishAllPorts || c.Image != "sha256:actual" || c.Config.Image != "repo@sha256:manifest" ||
		c.NetworkSettings.Networks["outside"].GlobalIPv6Address != "fd00::2" || len(c.Mounts) != 1 || !c.Mounts[0].RW {
		t.Fatalf("inspect omitted sandbox evidence: %v", err)
	}
}
