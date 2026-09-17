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

func testPublicBundle(t *testing.T) runtimebootstrap.PublicBundle {
	t.Helper()
	ca, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, certificate, err := ca.IssueServer([]string{"test.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	return runtimebootstrap.PublicBundle{CertificatePEM: certificate.CertificatePEM, CAPEM: ca.CertificatePEM()}
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
	bundle := testPublicBundle(t)
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
		{frame(2, "runtime bootstrap rejected\n"), 2, runtimebootstrap.ErrBootstrap},
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

func TestTypedInstallExplicitRejectionRequiresExactCompletion(t *testing.T) {
	bundle := testPublicBundle(t)
	rejected := frame(2, "runtime bootstrap rejected\n")
	for _, tc := range []struct {
		name        string
		output      string
		exit        int
		failSuffix  string
		inspectEdit map[string]any
		want        error
	}{
		{name: "exact rejection", output: rejected, exit: 2, want: runtimebootstrap.ErrInstallRejected},
		{name: "retry stays distinct", output: rejected, exit: 75, want: runtimebootstrap.ErrNotReady},
		{name: "wrong exit", output: rejected, exit: 1, want: runtimebootstrap.ErrBootstrap},
		{name: "zero exit", output: rejected, exit: 0, want: runtimebootstrap.ErrBootstrap},
		{name: "stdout present", output: frame(1, "unexpected") + rejected, exit: 2, want: runtimebootstrap.ErrBootstrap},
		{name: "wrong stderr", output: frame(2, "Authorization: secret"), exit: 2, want: runtimebootstrap.ErrBootstrap},
		{name: "stderr missing newline", output: frame(2, "runtime bootstrap rejected"), exit: 2, want: runtimebootstrap.ErrBootstrap},
		{name: "empty stream", exit: 2, want: runtimebootstrap.ErrBootstrap},
		{name: "bad frame", output: "malformed", exit: 2, want: runtimebootstrap.ErrBootstrap},
		{name: "create failure", output: rejected, exit: 2, failSuffix: "/exec", want: runtimebootstrap.ErrBootstrap},
		{name: "start failure", output: rejected, exit: 2, failSuffix: "/start", want: runtimebootstrap.ErrBootstrap},
		{name: "inspect failure", output: rejected, exit: 2, failSuffix: "/json", want: runtimebootstrap.ErrBootstrap},
		{name: "wrong CID", output: rejected, exit: 2, inspectEdit: map[string]any{"ContainerID": strings.Repeat("d", 64)}, want: runtimebootstrap.ErrBootstrap},
		{name: "wrong exec ID", output: rejected, exit: 2, inspectEdit: map[string]any{"ID": strings.Repeat("f", 64)}, want: runtimebootstrap.ErrBootstrap},
		{name: "still running", output: rejected, exit: 2, inspectEdit: map[string]any{"Running": true}, want: runtimebootstrap.ErrBootstrap},
		{name: "exit unavailable", output: rejected, exit: 2, inspectEdit: map[string]any{"ExitCode": nil}, want: runtimebootstrap.ErrBootstrap},
		{name: "running unavailable", output: rejected, exit: 2, inspectEdit: map[string]any{"Running": nil}, want: runtimebootstrap.ErrBootstrap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured createRequest
			base := testCall(t, tc.output, tc.exit, &captured)
			call := func(ctx context.Context, method, path string, body, output any) error {
				if tc.failSuffix != "" && strings.HasSuffix(path, tc.failSuffix) {
					return errors.New("Authorization: secret")
				}
				if err := base(ctx, method, path, body, output); err != nil {
					return err
				}
				if strings.HasSuffix(path, "/json") && tc.inspectEdit != nil {
					encoded, _ := json.Marshal(output)
					var fields map[string]any
					if err := json.Unmarshal(encoded, &fields); err != nil {
						return err
					}
					for key, value := range tc.inspectEdit {
						fields[key] = value
					}
					encoded, _ = json.Marshal(fields)
					return json.Unmarshal(encoded, output)
				}
				return nil
			}
			if err := Install(context.Background(), strings.Repeat("c", 64), 1000, bundle, call); err != tc.want {
				t.Fatal("installation failure was incorrectly classified", err)
			}
		})
	}
}

func TestTypedExecRejectsCancellationAfterInspect(t *testing.T) {
	bundle := testPublicBundle(t)
	for _, operation := range []string{"request", "install"} {
		for _, tc := range []struct {
			name   string
			output string
			exit   int
		}{
			{"explicit rejection", frame(2, "runtime bootstrap rejected\n"), 2},
			{"retry", frame(2, "runtime bootstrap rejected\n"), 75},
			{"success", frame(1, "bootstrap-installed\n"), 0},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var captured createRequest
				base := testCall(t, tc.output, tc.exit, &captured)
				call := func(ctx context.Context, method, path string, body, output any) error {
					if err := base(ctx, method, path, body, output); err != nil {
						return err
					}
					if strings.HasSuffix(path, "/json") {
						// A complete, identity-matching reply is not evidence once
						// the parent request has been cancelled during the I/O.
						cancel()
					}
					return nil
				}
				if operation == "request" {
					if value, err := Request(ctx, strings.Repeat("c", 64), 1000, call); err != runtimebootstrap.ErrBootstrap || len(value) != 0 {
						t.Fatal("cancelled request accepted completed exec evidence", err)
					}
				} else if err := Install(ctx, strings.Repeat("c", 64), 1000, bundle, call); err != runtimebootstrap.ErrBootstrap {
					t.Fatal("cancelled install accepted completed exec evidence", err)
				}
			})
		}
	}
}
