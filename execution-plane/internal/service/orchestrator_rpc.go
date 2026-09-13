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
	runners := []orchestratorBackgroundRunner{{
		runner: components.ProvisioningRunner, unexpectedExit: ErrProvisioningRun,
	}}
	if components.StartCoordinator != nil {
		runners = append(runners, orchestratorBackgroundRunner{
			runner: components.StartCoordinator, unexpectedExit: ErrOnboardingStartCoordinate,
		})
	}
	return superviseOrchestratorRPC(ctx, listener, server, runners)
}

type orchestratorRunner interface {
	Run(context.Context) error
}

type orchestratorBackgroundRunner struct {
	runner         orchestratorRunner
	unexpectedExit error
}

type orchestratorBackgroundResult struct {
	err            error
	unexpectedExit error
}

func superviseOrchestratorRPC(
	ctx context.Context,
	listener net.Listener,
	server *grpc.Server,
	runners []orchestratorBackgroundRunner,
) error {
	if ctx == nil || ctx.Err() != nil || listener == nil || server == nil || len(runners) == 0 {
		return ErrOrchestratorRPC
	}
	for _, background := range runners {
		if background.runner == nil || background.unexpectedExit == nil {
			return ErrOrchestratorRPC
		}
	}
	runtimeContext, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	backgroundResults := make(chan orchestratorBackgroundResult, len(runners))
	for _, background := range runners {
		background := background
		go func() {
			backgroundResults <- orchestratorBackgroundResult{
				err: background.runner.Run(runtimeContext), unexpectedExit: background.unexpectedExit,
			}
		}()
	}

	select {
	case err := <-serveResult:
		cancelRuntime()
		backgroundErr := awaitOrchestratorBackgrounds(backgroundResults, len(runners))
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
		if backgroundErr != nil {
			return backgroundErr
		}
		return nil
	case result := <-backgroundResults:
		cancelRuntime()
		server.Stop()
		serveErr := <-serveResult
		remainingErr := awaitOrchestratorBackgrounds(backgroundResults, len(runners)-1)
		if result.err != nil {
			return result.err
		}
		if ctx.Err() == nil {
			return result.unexpectedExit
		}
		if remainingErr != nil {
			return remainingErr
		}
		if errors.Is(serveErr, grpc.ErrServerStopped) {
			return nil
		}
		return serveErr
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
		backgroundErr := awaitOrchestratorBackgrounds(backgroundResults, len(runners))
		if backgroundErr != nil {
			return backgroundErr
		}
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

func awaitOrchestratorBackgrounds(results <-chan orchestratorBackgroundResult, count int) error {
	var firstErr error
	for range count {
		result := <-results
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
	}
	return firstErr
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
