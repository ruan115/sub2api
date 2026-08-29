package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxEngineResponseBytes = 1 << 20
const minimumEngineAPIVersion = "1.41"

type Engine interface {
	Ping(ctx context.Context) error
	InspectNetwork(ctx context.Context, networkID string) (Network, error)
	CreateNetwork(ctx context.Context, request CreateNetworkRequest) (CreateNetworkResponse, error)
	RemoveNetwork(ctx context.Context, networkID string) error
	CreateContainer(ctx context.Context, name string, request CreateContainerRequest) (CreateContainerResponse, error)
	InspectContainer(ctx context.Context, containerID string) (Container, error)
	StartContainer(ctx context.Context, containerID string) error
	KillContainer(ctx context.Context, containerID, signal string) error
	StopContainer(ctx context.Context, containerID string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, containerID string, force, removeVolumes bool) error
}

type HTTPConfig struct {
	SocketPath string
	UserAgent  string
}

func NewHTTPEngine(config HTTPConfig) (*HTTPEngine, error) {
	if strings.TrimSpace(config.SocketPath) == "" {
		return nil, errors.New("Docker Engine socket path is required")
	}
	if strings.TrimSpace(config.UserAgent) == "" {
		config.UserAgent = "sub2api-host-agent/development"
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", config.SocketPath)
		},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return &HTTPEngine{
		client:    &http.Client{Transport: transport},
		baseURL:   "http://docker",
		userAgent: config.UserAgent,
	}, nil
}

type HTTPEngine struct {
	client    *http.Client
	baseURL   string
	userAgent string

	mu        sync.RWMutex
	apiPrefix string
}

func (e *HTTPEngine) Ping(ctx context.Context) error {
	if err := e.doUnversioned(ctx, http.MethodHead, "/_ping", nil, nil); err != nil {
		if fallbackErr := e.doUnversioned(ctx, http.MethodGet, "/_ping", nil, nil); fallbackErr != nil {
			return fallbackErr
		}
	}
	var version Version
	if err := e.doUnversioned(ctx, http.MethodGet, "/version", nil, &version); err != nil {
		return fmt.Errorf("negotiate Docker Engine API: %w", err)
	}
	if compareAPIVersion(version.APIVersion, minimumEngineAPIVersion) < 0 {
		return fmt.Errorf("Docker Engine API %q is older than required %s", version.APIVersion, minimumEngineAPIVersion)
	}
	e.mu.Lock()
	e.apiPrefix = "/v" + version.APIVersion
	e.mu.Unlock()
	return nil
}

func (e *HTTPEngine) InspectNetwork(ctx context.Context, networkID string) (Network, error) {
	var network Network
	err := e.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(networkID), nil, &network)
	return network, err
}

func (e *HTTPEngine) CreateNetwork(ctx context.Context, request CreateNetworkRequest) (CreateNetworkResponse, error) {
	var response CreateNetworkResponse
	err := e.do(ctx, http.MethodPost, "/networks/create", request, &response)
	return response, err
}

func (e *HTTPEngine) RemoveNetwork(ctx context.Context, networkID string) error {
	return e.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(networkID), nil, nil)
}

func (e *HTTPEngine) CreateContainer(ctx context.Context, name string, request CreateContainerRequest) (CreateContainerResponse, error) {
	var response CreateContainerResponse
	path := "/containers/create?name=" + url.QueryEscape(name)
	err := e.do(ctx, http.MethodPost, path, request, &response)
	return response, err
}

func (e *HTTPEngine) InspectContainer(ctx context.Context, containerID string) (Container, error) {
	var container Container
	err := e.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(containerID)+"/json", nil, &container)
	return container, err
}

func (e *HTTPEngine) StartContainer(ctx context.Context, containerID string) error {
	return e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(containerID)+"/start", nil, nil)
}

func (e *HTTPEngine) KillContainer(ctx context.Context, containerID, signal string) error {
	path := "/containers/" + url.PathEscape(containerID) + "/kill?signal=" + url.QueryEscape(signal)
	return e.do(ctx, http.MethodPost, path, nil, nil)
}

func (e *HTTPEngine) StopContainer(ctx context.Context, containerID string, timeout time.Duration) error {
	seconds := strconv.Itoa(int((timeout + time.Second - 1) / time.Second))
	path := "/containers/" + url.PathEscape(containerID) + "/stop?t=" + url.QueryEscape(seconds)
	return e.do(ctx, http.MethodPost, path, nil, nil)
}

func (e *HTTPEngine) RemoveContainer(ctx context.Context, containerID string, force, removeVolumes bool) error {
	values := url.Values{}
	values.Set("force", strconv.FormatBool(force))
	values.Set("v", strconv.FormatBool(removeVolumes))
	path := "/containers/" + url.PathEscape(containerID) + "?" + values.Encode()
	return e.do(ctx, http.MethodDelete, path, nil, nil)
}

// ExecContainer is used by local sandbox verification to run a fixed helper
// already present in the test image. Production worker lifecycle does not
// depend on Docker exec and workers never receive the Docker socket.
func (e *HTTPEngine) ExecContainer(ctx context.Context, containerID string, command []string) (ExecResult, error) {
	if strings.TrimSpace(containerID) == "" || len(command) == 0 || len(command) > 64 {
		return ExecResult{}, errors.New("Docker exec request is invalid")
	}
	for _, argument := range command {
		if len(argument) == 0 || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return ExecResult{}, errors.New("Docker exec command is invalid")
		}
	}
	var created CreateExecResponse
	if err := e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(containerID)+"/exec", CreateExecRequest{
		AttachStdout: true,
		AttachStderr: true,
		User:         "65532:65532",
		Cmd:          append([]string(nil), command...),
	}, &created); err != nil {
		return ExecResult{}, fmt.Errorf("create Docker exec: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return ExecResult{}, errors.New("Docker exec returned an empty id")
	}
	var raw string
	if err := e.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", StartExecRequest{}, &raw); err != nil {
		return ExecResult{}, fmt.Errorf("start Docker exec: %w", err)
	}
	stdout, stderr, err := decodeDockerStream([]byte(raw))
	if err != nil {
		return ExecResult{}, err
	}
	var inspected ExecInspect
	if err := e.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, &inspected); err != nil {
		return ExecResult{}, fmt.Errorf("inspect Docker exec: %w", err)
	}
	if inspected.Running {
		return ExecResult{}, errors.New("Docker exec remained running after attached start")
	}
	return ExecResult{ExitCode: inspected.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

func decodeDockerStream(payload []byte) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	for len(payload) != 0 {
		if len(payload) < 8 {
			return nil, nil, errors.New("Docker exec stream header is truncated")
		}
		stream := payload[0]
		length := uint64(payload[4])<<24 | uint64(payload[5])<<16 | uint64(payload[6])<<8 | uint64(payload[7])
		payload = payload[8:]
		if length > uint64(len(payload)) {
			return nil, nil, errors.New("Docker exec stream payload is truncated")
		}
		frame := payload[:int(length)]
		payload = payload[int(length):]
		switch stream {
		case 1:
			_, _ = stdout.Write(frame)
		case 2:
			_, _ = stderr.Write(frame)
		default:
			return nil, nil, errors.New("Docker exec stream type is invalid")
		}
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func (e *HTTPEngine) do(ctx context.Context, method, path string, requestBody, responseBody any) error {
	e.mu.RLock()
	prefix := e.apiPrefix
	e.mu.RUnlock()
	if prefix == "" {
		return errors.New("Docker Engine API version has not been negotiated")
	}
	return e.doUnversioned(ctx, method, prefix+path, requestBody, responseBody)
}

func (e *HTTPEngine) doUnversioned(ctx context.Context, method, path string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode Docker Engine request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Docker Engine request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", e.userAgent)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("call Docker Engine: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxEngineResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Docker Engine response: %w", err)
	}
	if len(payload) > maxEngineResponseBytes {
		return errors.New("Docker Engine response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var engineError struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &engineError)
		return &APIError{StatusCode: response.StatusCode, Message: engineError.Message}
	}
	if responseBody == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if target, ok := responseBody.(*string); ok {
		*target = string(payload)
		return nil
	}
	if err := json.Unmarshal(payload, responseBody); err != nil {
		return fmt.Errorf("decode Docker Engine response: %w", err)
	}
	return nil
}

type Version struct {
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
}

func compareAPIVersion(left, right string) int {
	parse := func(value string) (int, int, bool) {
		parts := strings.Split(value, ".")
		if len(parts) != 2 {
			return 0, 0, false
		}
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		return major, minor, majorErr == nil && minorErr == nil
	}
	leftMajor, leftMinor, leftOK := parse(left)
	rightMajor, rightMinor, rightOK := parse(right)
	if !leftOK || !rightOK {
		return -1
	}
	if leftMajor != rightMajor {
		if leftMajor < rightMajor {
			return -1
		}
		return 1
	}
	if leftMinor < rightMinor {
		return -1
	}
	if leftMinor > rightMinor {
		return 1
	}
	return 0
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Docker Engine returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("Docker Engine returned HTTP %d: %s", e.StatusCode, e.Message)
}

func IsNotFound(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound
}

func IsConflict(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusConflict
}

func IsNotModified(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotModified
}

type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
	IPAM     NetworkIPAM       `json:"IPAM"`
}

type NetworkIPAM struct {
	Config []NetworkIPAMConfig `json:"Config"`
}

type NetworkIPAMConfig struct {
	Gateway string `json:"Gateway"`
}

type CreateNetworkRequest struct {
	Name           string            `json:"Name"`
	CheckDuplicate bool              `json:"CheckDuplicate"`
	Driver         string            `json:"Driver"`
	Internal       bool              `json:"Internal"`
	Attachable     bool              `json:"Attachable"`
	Labels         map[string]string `json:"Labels"`
}

type CreateNetworkResponse struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning"`
}

type CreateContainerRequest struct {
	Image        string              `json:"Image"`
	Hostname     string              `json:"Hostname"`
	User         string              `json:"User"`
	Labels       map[string]string   `json:"Labels"`
	Env          []string            `json:"Env"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	StopTimeout  *int                `json:"StopTimeout"`
	HostConfig   HostConfig          `json:"HostConfig"`
}

type HostConfig struct {
	NetworkMode    string                   `json:"NetworkMode"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	CapDrop        []string                 `json:"CapDrop"`
	SecurityOpt    []string                 `json:"SecurityOpt"`
	PidsLimit      int64                    `json:"PidsLimit"`
	Memory         int64                    `json:"Memory"`
	NanoCPUs       int64                    `json:"NanoCpus"`
	Tmpfs          map[string]string        `json:"Tmpfs"`
	Init           *bool                    `json:"Init"`
	RestartPolicy  RestartPolicy            `json:"RestartPolicy"`
	LogConfig      LogConfig                `json:"LogConfig"`
	ExtraHosts     []string                 `json:"ExtraHosts"`
	Binds          []string                 `json:"Binds,omitempty"`
	PortBindings   map[string][]PortBinding `json:"PortBindings,omitempty"`
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type CreateContainerResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type CreateExecRequest struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	User         string   `json:"User"`
	Cmd          []string `json:"Cmd"`
}

type CreateExecResponse struct {
	ID string `json:"Id"`
}

type StartExecRequest struct {
	Detach bool `json:"Detach"`
	TTY    bool `json:"Tty"`
}

type ExecInspect struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type Container struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
		User   string            `json:"User"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	HostConfig      HostConfig `json:"HostConfig"`
	NetworkSettings struct {
		Ports    map[string][]PortBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	AppArmorProfile string `json:"AppArmorProfile"`
	Mounts          []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	State ContainerState `json:"State"`
}

type ContainerState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
	Health  *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}
