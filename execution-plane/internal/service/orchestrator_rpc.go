package service

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	orchestratorRPCMaxMessageBytes = 3 << 20
	orchestratorRPCStopTimeout     = 10 * time.Second
)

var ErrOrchestratorRPC = errors.New("orchestrator RPC server configuration is invalid")

// RunOrchestratorRPC serves NodeControl and the CCMAX onboarding intake on a
// shared TLS listener. Client certificates are verified when presented rather
// than universally required: one-time node enrollment starts without a client
// certificate, while NodeControl and intake methods enforce their exact peer
// identities after the TLS handshake.
func RunOrchestratorRPC(
	ctx context.Context,
	listener net.Listener,
	tlsConfig *tls.Config,
	components *OrchestratorComponents,
) error {
	if ctx == nil || ctx.Err() != nil || listener == nil || validateOrchestratorTLS(tlsConfig) != nil || components == nil {
		return ErrOrchestratorRPC
	}
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig.Clone())),
		grpc.MaxRecvMsgSize(orchestratorRPCMaxMessageBytes),
		grpc.MaxSendMsgSize(orchestratorRPCMaxMessageBytes),
	)
	if err := components.Register(server); err != nil {
		return ErrOrchestratorRPC
	}
	runtimeContext, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	runnerResult := make(chan error, 1)
	go func() { runnerResult <- components.ProvisioningRunner.Run(runtimeContext) }()

	select {
	case err := <-serveResult:
		cancelRuntime()
		<-runnerResult
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case err := <-runnerResult:
		server.Stop()
		<-serveResult
		if err == nil && ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return ErrProvisioningRun
		}
		return err
	case <-ctx.Done():
		cancelRuntime()
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()
		timer := time.NewTimer(orchestratorRPCStopTimeout)
		defer timer.Stop()
		select {
		case <-stopped:
		case <-timer.C:
			server.Stop()
			<-stopped
		}
		err := <-serveResult
		<-runnerResult
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

func validateOrchestratorTLS(config *tls.Config) error {
	if config == nil || config.MinVersion != tls.VersionTLS13 ||
		(config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS13) ||
		config.ClientAuth != tls.VerifyClientCertIfGiven || config.ClientCAs == nil || len(config.ClientCAs.Subjects()) == 0 ||
		(len(config.Certificates) == 0 && config.GetCertificate == nil) {
		return ErrOrchestratorRPC
	}
	return nil
}
