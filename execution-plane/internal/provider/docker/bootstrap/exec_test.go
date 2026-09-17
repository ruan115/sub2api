package bootstrap

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

func frame(stream byte, value string) string {
	data := make([]byte, 8+len(value))
	data[0] = stream
	binary.BigEndian.PutUint32(data[4:8], uint32(len(value)))
	copy(data[8:], value)
	return string(data)
}

func testCall(t *testing.T, out string, exit int, capture *createRequest) Call {
	t.Helper()
	return func(ctx context.Context, method, path string, body, output any) error {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded exec")
		}
		switch {
		case strings.HasSuffix(path, "/exec"):
			if method != "POST" || path != "/containers/"+strings.Repeat("c", 64)+"/exec" {
				t.Fatal("unbound create target")
			}
			encoded, _ := json.Marshal(body)
			if err := json.Unmarshal(encoded, capture); err != nil {
				t.Fatal(err)
			}
			return json.Unmarshal([]byte(`{"Id":"`+strings.Repeat("e", 64)+`"}`), output)
		case strings.HasSuffix(path, "/start"):
			if method != "POST" || path != "/exec/"+strings.Repeat("e", 64)+"/start" {
				t.Fatal("unbound start")
			}
			*(output.(*string)) = out
			return nil
		case strings.HasSuffix(path, "/json"):
			encoded, _ := json.Marshal(map[string]any{"ID": strings.Repeat("e", 64), "ContainerID": strings.Repeat("c", 64), "Running": false, "ExitCode": exit})
			return json.Unmarshal(encoded, output)
		default:
			t.Fatal("unexpected request")
			return nil
		}
	}
}

func TestTypedExecUsesOnlyFixedNonRootCommands(t *testing.T) {
	var captured createRequest
	value, err := Request(context.Background(), strings.Repeat("c", 64), 1000, testCall(t, frame(1, "public request"), 0, &captured))
	if err != nil || string(value) != "public request" || captured.User != "1000:1000" || !captured.AttachStdout || !captured.AttachStderr ||
		len(captured.Cmd) != 2 || captured.Cmd[0] != "/worker" || captured.Cmd[1] != "bootstrap-request" {
		t.Fatal("unsafe request command")
	}
	ca, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, certificate, err := ca.IssueServer([]string{"test.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	bundle := runtimebootstrap.PublicBundle{CertificatePEM: certificate.CertificatePEM, CAPEM: ca.CertificatePEM()}
	err = Install(context.Background(), strings.Repeat("c", 64), 1000, bundle, testCall(t, frame(1, "bootstrap-installed\n"), 0, &captured))
	if err != nil || captured.User != "1000:1000" || len(captured.Cmd) != 3 || captured.Cmd[0] != "/worker" || captured.Cmd[1] != "bootstrap-install" {
		t.Fatal("unsafe install command", err)
	}
	if decoded, err := runtimebootstrap.DecodeBundleArgument(captured.Cmd[2]); err != nil || string(decoded.CertificatePEM) != string(bundle.CertificatePEM) {
		t.Fatal("install did not carry exact public bundle")
	}
}

func TestTypedExecRejectsUnsafeTargetsAndNormalizesErrors(t *testing.T) {
	calls := 0
	call := func(context.Context, string, string, any, any) error {
		calls++
		return errors.New("Authorization: secret")
	}
	for _, target := range []string{"worker-name", "../containers", strings.Repeat("C", 64), ""} {
		if _, err := Request(context.Background(), target, 1000, call); err != runtimebootstrap.ErrBootstrap {
			t.Fatal("invalid target accepted")
		}
	}
	if _, err := Request(context.Background(), strings.Repeat("c", 64), 0, call); err != runtimebootstrap.ErrBootstrap {
		t.Fatal("root exec accepted")
	}
	if calls != 0 {
		t.Fatal("invalid input reached Engine")
	}
	if _, err := Request(context.Background(), strings.Repeat("c", 64), 1000, call); err != runtimebootstrap.ErrBootstrap {
		t.Fatal("Engine error leaked")
	}
	var captured createRequest
	for _, tc := range []struct {
		output string
		exit   int
		want   error
	}{
		{frame(2, "runtime bootstrap rejected\n"), 75, runtimebootstrap.ErrNotReady},
		{frame(1, "unexpected") + frame(2, "runtime bootstrap rejected\n"), 75, runtimebootstrap.ErrBootstrap},
		{frame(2, "secret from stderr"), 2, runtimebootstrap.ErrBootstrap},
		{frame(1, strings.Repeat("a", runtimebootstrap.MaxPublicBytes+1)), 0, runtimebootstrap.ErrBootstrap},
		{"invalid-frame", 0, runtimebootstrap.ErrBootstrap},
		{frame(3, "unrecognized"), 0, runtimebootstrap.ErrBootstrap},
	} {
		if _, err := Request(context.Background(), strings.Repeat("c", 64), 1000, testCall(t, tc.output, tc.exit, &captured)); err != tc.want {
			t.Fatal("unexpected bounded exec result", err)
		}
	}
}
