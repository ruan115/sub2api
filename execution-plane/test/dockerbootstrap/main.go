package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
)

func run(input io.Reader, output io.Writer) string {
	scanner := inputScanner(input)
	var config initialInput
	if readLine(scanner, &config, "owner", "image_id", "worker_path") != nil || config.validate() != nil {
		return "input"
	}
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		return "authority"
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "authority"
	}
	pin := sha256.Sum256(authority.CertificatePEM())
	public := publicConfig{TrustSHA256: hex.EncodeToString(pin[:]), TicketPublicKey: base64.RawStdEncoding.EncodeToString(publicKey)}
	world, err := newControlWorld(authority)
	if err != nil {
		return "control"
	}
	defer world.close()
	encoder := json.NewEncoder(output)
	if encoder.Encode(public) != nil {
		return "output"
	}
	var containers containerInput
	if readLine(scanner, &containers, "container_ids") != nil || containers.validate() != nil {
		return "containers"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	engine, err := docker.NewHTTPEngine(docker.HTTPConfig{SocketPath: "/var/run/docker.sock", UserAgent: "sub2api-trusted-live-bootstrap-lab"})
	if err != nil {
		return "engine"
	}
	e := experiment{config: config, public: public, ids: containers.ContainerIDs, engine: engine, world: world}
	result, err := e.run(ctx)
	if err != nil || ctx.Err() != nil {
		return e.stage
	}
	if encoder.Encode(result) != nil {
		return "output"
	}
	return ""
}

func main() {
	// This executable never prints a wrapped dependency error, certificate,
	// CSR, nonce, key, Docker response, or Go panic stack.
	defer func() {
		if recover() != nil {
			_, _ = io.WriteString(os.Stderr, "docker bootstrap probe rejected: internal\n")
			os.Exit(1)
		}
	}()
	if len(os.Args) != 1 {
		_, _ = io.WriteString(os.Stderr, "docker bootstrap probe rejected: arguments\n")
		os.Exit(1)
	}
	if stage := run(os.Stdin, os.Stdout); stage != "" {
		_, _ = io.WriteString(os.Stderr, "docker bootstrap probe rejected: "+stage+"\n")
		os.Exit(1)
	}
}
