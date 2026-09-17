// Package bootstrap implements exactly two non-root Docker management commands.
// It never accepts a caller-selected executable, shell or arbitrary arguments.
package bootstrap

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

type Call func(context.Context, string, string, any, any) error

var idPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type createRequest struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	User         string   `json:"User"`
	Cmd          []string `json:"Cmd"`
}

func Request(ctx context.Context, cid string, uid uint32, call Call) ([]byte, error) {
	return execute(ctx, cid, uid, []string{"/worker", "bootstrap-request"}, call, false)
}

func Install(ctx context.Context, cid string, uid uint32, bundle runtimebootstrap.PublicBundle, call Call) error {
	argument, err := runtimebootstrap.EncodeBundleArgument(bundle)
	if err != nil {
		return runtimebootstrap.ErrBootstrap
	}
	output, err := execute(ctx, cid, uid, []string{"/worker", "bootstrap-install", argument}, call, true)
	if err != nil {
		return err
	}
	if string(output) != "bootstrap-installed\n" {
		return runtimebootstrap.ErrBootstrap
	}
	return nil
}

func execute(ctx context.Context, cid string, uid uint32, command []string, call Call, install bool) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || !idPattern.MatchString(cid) || uid == 0 || call == nil {
		return nil, runtimebootstrap.ErrBootstrap
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var created struct {
		ID string `json:"Id"`
	}
	user := strconv.FormatUint(uint64(uid), 10)
	if call(ctx, "POST", "/containers/"+cid+"/exec", createRequest{AttachStdout: true, AttachStderr: true, User: user + ":" + user, Cmd: command}, &created) != nil || !idPattern.MatchString(created.ID) {
		return nil, runtimebootstrap.ErrBootstrap
	}
	var raw string
	if call(ctx, "POST", "/exec/"+created.ID+"/start", struct {
		Detach bool `json:"Detach"`
		TTY    bool `json:"Tty"`
	}{}, &raw) != nil {
		return nil, runtimebootstrap.ErrBootstrap
	}
	stdout, stderr, err := decode([]byte(raw))
	if err != nil {
		return nil, runtimebootstrap.ErrBootstrap
	}
	var inspected struct {
		ID          string `json:"ID"`
		ContainerID string `json:"ContainerID"`
		Running     *bool  `json:"Running"`
		ExitCode    *int   `json:"ExitCode"`
	}
	if call(ctx, "GET", "/exec/"+created.ID+"/json", nil, &inspected) != nil || ctx.Err() != nil || inspected.Running == nil || inspected.ExitCode == nil ||
		*inspected.Running || inspected.ID != created.ID || inspected.ContainerID != cid {
		return nil, runtimebootstrap.ErrBootstrap
	}
	if *inspected.ExitCode == 75 && len(stdout) == 0 && string(stderr) == "runtime bootstrap rejected\n" {
		return nil, runtimebootstrap.ErrNotReady
	}
	if install && *inspected.ExitCode == 2 && len(stdout) == 0 && string(stderr) == "runtime bootstrap rejected\n" {
		return nil, runtimebootstrap.ErrInstallRejected
	}
	if *inspected.ExitCode != 0 || len(stderr) != 0 || len(stdout) == 0 {
		return nil, runtimebootstrap.ErrBootstrap
	}
	return stdout, nil
}

func decode(raw []byte) ([]byte, []byte, error) {
	if len(raw) == 0 || len(raw) > runtimebootstrap.MaxPublicBytes+4096 {
		return nil, nil, runtimebootstrap.ErrBootstrap
	}
	var stdout, stderr bytes.Buffer
	for len(raw) > 0 {
		if len(raw) < 8 || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
			return nil, nil, runtimebootstrap.ErrBootstrap
		}
		stream := raw[0]
		length := uint64(raw[4])<<24 | uint64(raw[5])<<16 | uint64(raw[6])<<8 | uint64(raw[7])
		raw = raw[8:]
		if length > uint64(len(raw)) {
			return nil, nil, runtimebootstrap.ErrBootstrap
		}
		data := raw[:int(length)]
		raw = raw[int(length):]
		switch stream {
		case 1:
			stdout.Write(data)
		case 2:
			stderr.Write(data)
		default:
			return nil, nil, runtimebootstrap.ErrBootstrap
		}
		if stdout.Len() > runtimebootstrap.MaxPublicBytes || stderr.Len() > 64 {
			return nil, nil, runtimebootstrap.ErrBootstrap
		}
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
