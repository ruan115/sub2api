package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

// No daemon, worker executable, private-key file or external network is used.
// Only the TLS bufconn Control service is real in this unit test.
type fakeLiveEngine struct {
	config                                                initialInput
	public                                                publicConfig
	ids                                                   []string
	keys                                                  [2]*ecdsa.PrivateKey
	installed                                             [2]bool
	inspectCount, requestCount, installCount, healthCount int
	drift                                                 bool
	acceptWrong                                           bool
	badHealth                                             bool
	installOverride                                       func() error
	inspectFailureAt                                      int
}

func (f *fakeLiveEngine) index(cid string) (int, error) {
	for i, v := range f.ids {
		if v == cid {
			return i, nil
		}
	}
	return 0, rejected
}
func (f *fakeLiveEngine) Ping(context.Context) error { return nil }
func (f *fakeLiveEngine) InspectContainer(_ context.Context, cid string) (docker.Container, error) {
	i, err := f.index(cid)
	if err != nil {
		return docker.Container{}, err
	}
	f.inspectCount++
	if f.inspectFailureAt == f.inspectCount {
		return docker.Container{}, runtimebootstrap.ErrBootstrap
	}
	c := containerFixture(f.config, f.public, cid, i)
	if f.drift {
		c.HostConfig.Privileged = true
	}
	return c, nil
}
func (f *fakeLiveEngine) BootstrapRequestExec(_ context.Context, cid string, uid uint32) ([]byte, error) {
	i, err := f.index(cid)
	if err != nil || uid != 1000 {
		return nil, rejected
	}
	f.requestCount++
	u, _ := binding(i).URI()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}}, f.keys[i])
	if err != nil {
		return nil, err
	}
	return json.Marshal(runtimebootstrap.Request{Binding: binding(i), CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})})
}
func (f *fakeLiveEngine) BootstrapInstallExec(_ context.Context, cid string, uid uint32, bundle runtimebootstrap.PublicBundle) error {
	i, err := f.index(cid)
	if err != nil || uid != 1000 {
		return rejected
	}
	f.installCount++
	if f.installOverride != nil {
		return f.installOverride()
	}
	pin := sha256.Sum256(bundle.CAPEM)
	leaf, err := runtimeidentity.ValidateCertificate(binding(i), bundle.CertificatePEM, bundle.CAPEM, time.Now())
	if hex.EncodeToString(pin[:]) != f.public.TrustSHA256 || err != nil {
		if f.acceptWrong {
			return nil
		}
		return runtimebootstrap.ErrInstallRejected
	}
	want, _ := x509.MarshalPKIXPublicKey(&f.keys[i].PublicKey)
	got, _ := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if !bytes.Equal(want, got) {
		return runtimebootstrap.ErrInstallRejected
	}
	f.installed[i] = true
	return nil
}

func TestRejectInstallRequiresExplicitWorkerRejection(t *testing.T) {
	for _, mode := range []string{"worker-rejected", "transport-failed", "success", "cancelled", "pre-inspect-failed", "post-inspect-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeLiveEngine{config: testConfig(), public: testPublic(), ids: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			f.installOverride = func() error { return runtimebootstrap.ErrInstallRejected }
			switch mode {
			case "transport-failed":
				f.installOverride = func() error { return runtimebootstrap.ErrBootstrap }
			case "success":
				f.installOverride = func() error { return nil }
			case "cancelled":
				f.installOverride = func() error { cancel(); return runtimebootstrap.ErrInstallRejected }
			case "pre-inspect-failed":
				f.inspectFailureAt = 1
			case "post-inspect-failed":
				f.inspectFailureAt = 2
			}
			e := experiment{config: f.config, public: f.public, ids: f.ids, engine: f}
			err := e.rejectInstall(ctx, 0, runtimebootstrap.PublicBundle{})
			if (err == nil) != (mode == "worker-rejected") {
				t.Fatal("non-worker failure counted as a successful negative")
			}
			if mode == "pre-inspect-failed" && f.installCount != 0 {
				t.Fatal("unverified container reached exec")
			}
		})
	}
}
func (f *fakeLiveEngine) ExecContainer(_ context.Context, cid string, command []string) (docker.ExecResult, error) {
	i, err := f.index(cid)
	if err != nil || !reflect.DeepEqual(command, []string{"/worker", "healthcheck"}) {
		return docker.ExecResult{}, rejected
	}
	f.healthCount++
	code := 1
	if f.installed[i] {
		code = 0
	}
	if f.badHealth {
		code = 2
	}
	return docker.ExecResult{ExitCode: code}, nil
}

func TestSyntheticEngineUsesRealAuthenticatedControlAndReceiptGates(t *testing.T) {
	for _, mode := range []string{"pass", "policy-drift", "incorrect-negative", "health-error"} {
		t.Run(mode, func(t *testing.T) {
			a, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			w, err := newControlWorld(a)
			if err != nil {
				t.Fatal("control fixture")
			}
			defer w.close()
			public := testPublic()
			pin := sha256.Sum256(a.CertificatePEM())
			public.TrustSHA256 = hex.EncodeToString(pin[:])
			f := &fakeLiveEngine{config: testConfig(), public: public, ids: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}, drift: mode == "policy-drift", acceptWrong: mode == "incorrect-negative", badHealth: mode == "health-error"}
			for i := range f.keys {
				f.keys[i], err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
			}
			e := experiment{config: f.config, public: public, ids: f.ids, engine: f, world: w}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := e.run(ctx)
			if mode != "pass" {
				if err == nil || got.ActualDockerExec {
					t.Fatal("failure published success")
				}
				if mode == "policy-drift" && f.requestCount != 0 {
					t.Fatal("drift reached exec")
				}
				return
			}
			if err != nil {
				t.Fatalf("synthetic exercise phase %s", e.stage)
			}
			if !got.ActualDockerExec || !got.AuthenticatedEnrollment || !got.IndependentKeys || !got.SameKeyLeafStable || !got.CrossInstallRejected || !got.WrongCARejected || !got.LeaseRetryRejected || !got.PublicInstallRetry || !got.TCPReady || got.CrossContainerMTLS || got.ProductionReady {
				t.Fatal("incorrect evidence boundaries")
			}
			if f.requestCount != 4 || f.installCount != 6 || f.healthCount != 6 || f.inspectCount != 2*(f.requestCount+f.installCount+f.healthCount)+2 {
				t.Fatalf("unexpected bounded operations: request=%d install=%d health=%d inspect=%d", f.requestCount, f.installCount, f.healthCount, f.inspectCount)
			}
		})
	}
}
