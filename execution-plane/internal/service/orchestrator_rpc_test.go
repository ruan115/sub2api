package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestRunOrchestratorRPCServesEnrollmentAndProtectsIntake(t *testing.T) {
	now := time.Now().UTC()
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, _, err := authority.IssueServer([]string{"orchestrator.test"})
	if err != nil {
		t.Fatal(err)
	}
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x71}, 32), "kms-rpc", "v1")
	runtimeRepository := store.NewMemoryRepository()
	rotationRecipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x53}, 32)))
	components, err := NewOrchestratorComponents(OrchestratorComponentsConfig{
		NodeRepository: runtimeRepository, CredentialRepository: runtimeRepository,
		IntentRepository: onboarding.NewMemoryRepository(), ProvisioningRepository: &componentDurableProvisioningRepository{MemoryProvisioningRepository: onboarding.NewMemoryProvisioningRepository()},
		Authority: authority, KMS: kms, RotationAuthorizer: componentRotationAuthorizer{}, RotationRecipient: rotationRecipient, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer components.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverTLS := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: authority.CertificatePool(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- RunOrchestratorRPC(ctx, listener, serverTLS, components) }()

	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(authority.CertificatePEM())
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: clientPool, ServerName: "orchestrator.test",
	})))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer connection.Close()
	rpcContext, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rpcCancel()
	_, err = executionv1.NewNodeControlServiceClient(connection).EnrollNode(rpcContext, &executionv1.EnrollNodeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		cancel()
		t.Fatalf("unauthenticated enrollment error = %v", err)
	}
	_, err = executionv1.NewOnboardingIntakeServiceClient(connection).CreateOnboardingIntent(rpcContext, &executionv1.CreateOnboardingIntentRequest{})
	if status.Code(err) != codes.PermissionDenied {
		cancel()
		t.Fatalf("certificate-free intake error = %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("RunOrchestratorRPC() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOrchestratorRPC did not stop after cancellation")
	}
}

func TestRunOrchestratorRPCRejectsTLSWithoutOptionalVerifiedClients(t *testing.T) {
	for _, config := range []*tls.Config{
		nil,
		{MinVersion: tls.VersionTLS12},
		{MinVersion: tls.VersionTLS13, ClientAuth: tls.NoClientCert},
		{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: x509.NewCertPool()},
	} {
		if err := validateOrchestratorTLS(config); err != ErrOrchestratorRPC {
			t.Fatalf("invalid TLS configuration error = %v", err)
		}
	}
}
