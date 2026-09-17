package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

// Fixed management commands carry public material only. There is no shell,
// arbitrary path, private-key argument, caller-selected identity or CA override.
func runBootstrap(args []string, getenv func(string) string, output io.Writer) error {
	if os.Geteuid() == 0 || len(args) == 0 {
		return runtimebootstrap.ErrBootstrap
	}
	request := len(args) == 1 && args[0] == "bootstrap-request"
	install := len(args) == 2 && args[0] == "bootstrap-install"
	if !request && !install {
		return runtimebootstrap.ErrBootstrap
	}
	config, err := worker.LoadProcessConfig(getenv)
	if err != nil || config.BootstrapConfig().Validate() != nil {
		return runtimebootstrap.ErrBootstrap
	}
	if request {
		value, err := runtimebootstrap.RequestIdentity(config.BootstrapConfig())
		if err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil || len(data)+1 > runtimebootstrap.MaxPublicBytes {
			return runtimebootstrap.ErrBootstrap
		}
		if n, err := output.Write(append(data, '\n')); err != nil || n != len(data)+1 {
			return runtimebootstrap.ErrBootstrap
		}
		return nil
	}
	bundle, err := runtimebootstrap.DecodeBundleArgument(args[1])
	if err != nil {
		return runtimebootstrap.ErrBootstrap
	}
	if err := runtimebootstrap.Install(config.BootstrapConfig(), bundle); err != nil {
		return err
	}
	if n, err := io.WriteString(output, "bootstrap-installed\n"); err != nil || n != len("bootstrap-installed\n") {
		return runtimebootstrap.ErrBootstrap
	}
	return nil
}
