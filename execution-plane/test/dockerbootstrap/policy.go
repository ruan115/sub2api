package main

import (
	"context"
	"reflect"
	"strings"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func binding(index int) runtimeidentity.Binding {
	suffix := []string{"a", "b"}[index]
	return runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID("account-live-" + suffix), SlotID: "slot-live-" + suffix, NodeID: nodeID, Epoch: 1, Generation: 1}
}

var tmpfs = map[string]string{
	"/tmp":         "rw,noexec,nosuid,nodev,size=64m,mode=1777",
	"/run":         "rw,noexec,nosuid,nodev,size=16m,mode=1777",
	"/home/claude": "rw,noexec,nosuid,nodev,size=128m,mode=0700,uid=1000,gid=1000",
}

func expectedEnvironment(index int, public publicConfig) map[string]string {
	b := binding(index)
	return map[string]string{
		"HOME": "/home/claude", "USER": "claude", "LOGNAME": "claude", "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
		"CLAUDE_CONFIG_DIR": "/home/claude/.claude", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "BUN_INSTALL_CACHE_DIR": "/home/claude/.bun-cache",
		"EXECUTION_LISTEN_ADDRESS": "127.0.0.1:8093", "EXECUTION_HEALTHCHECK_ADDRESS": "127.0.0.1:8093", "EXECUTION_ACCOUNT_HASH": b.AccountHash,
		"EXECUTION_SLOT_ID": b.SlotID, "EXECUTION_NODE_ID": nodeID, "EXECUTION_EPOCH": "1", "EXECUTION_RUNTIME_GENERATION": "1", "EXECUTION_IMAGE_DIGEST": baseImage,
		"EXECUTION_IDENTITY_DIRECTORY": "/run/execution/identity", "EXECUTION_RUNTIME_TRUST_FILE": "/run/execution/runtime-ca.pem",
		"EXECUTION_BOOTSTRAP_CA_SHA256": public.TrustSHA256, "EXECUTION_TICKET_PUBLIC_KEY": public.TicketPublicKey,
		"EXECUTION_UPSTREAM_BASE_URL": "https://api.anthropic.com", "EXECUTION_EGRESS_PROXY_URL": "http://host-agent.execution.internal:8094", "EXECUTION_ALLOW_FAKE_ACTIVATION": "true",
	}
}

// Python Lab additionally checks fields absent from the production Engine
// projection (MemorySwap, core ulimit, entrypoint and command). Never mutate an
// inspect result to make the production provider accept this lab-only profile.
func validateContainer(c docker.Container, config initialInput, public publicConfig, cid string, index int) error {
	h := c.HostConfig
	name := config.Owner + "-live-" + []string{"a", "b"}[index]
	if c.ID != cid || !cidPattern.MatchString(cid) || c.Name != "/"+name || c.Config.Hostname != name || c.Image != baseImage || c.Config.Image != baseImage || c.Config.User != "1000:1000" || c.Config.Labels[ownerLabel] != config.Owner || !c.State.Running {
		return rejected
	}
	if h.Privileged || !h.ReadonlyRootfs || h.NetworkMode != "none" || h.Memory != 1<<30 || h.NanoCPUs != 1_000_000_000 || h.PidsLimit != 128 ||
		len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges"}) ||
		(h.PidMode != "" && h.PidMode != "private") || h.IpcMode != "private" || h.CgroupnsMode != "private" || h.UTSMode != "" || h.UsernsMode != "" ||
		h.RestartPolicy.Name != "no" || h.PublishAllPorts || len(h.PortBindings) != 0 || len(c.NetworkSettings.Ports) != 0 ||
		len(h.Mounts) != 0 || len(h.VolumesFrom) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.DeviceCgroupRules) != 0 || len(h.Links) != 0 || len(h.ExtraHosts) != 0 ||
		!reflect.DeepEqual(h.Tmpfs, tmpfs) || !reflect.DeepEqual(h.Binds, []string{config.WorkerPath + ":/worker:ro"}) {
		return rejected
	}
	if len(c.NetworkSettings.Networks) != 1 {
		return rejected
	}
	endpoint, ok := c.NetworkSettings.Networks["none"]
	if !ok || endpoint.IPAddress != "" || endpoint.Gateway != "" || endpoint.GlobalIPv6Address != "" {
		return rejected
	}
	binds := 0
	seen := map[string]bool{}
	for _, mount := range c.Mounts {
		if seen[mount.Destination] {
			return rejected
		}
		seen[mount.Destination] = true
		if mount.Type == "bind" {
			if mount.Source != config.WorkerPath || mount.Destination != "/worker" || mount.RW || mount.Propagation != "rprivate" {
				return rejected
			}
			binds++
		} else if mount.Type != "tmpfs" || tmpfs[mount.Destination] == "" {
			return rejected
		}
	}
	if binds != 1 {
		return rejected
	}
	actual := map[string]string{}
	for _, item := range c.Config.Env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			return rejected
		}
		if _, exists := actual[key]; exists {
			return rejected
		}
		actual[key] = value
	}
	if !reflect.DeepEqual(actual, expectedEnvironment(index, public)) {
		return rejected
	}
	return nil
}

type inspector interface {
	InspectContainer(context.Context, string) (docker.Container, error)
}

func inspect(ctx context.Context, engine inspector, c initialInput, public publicConfig, ids []string, index int) error {
	if ctx.Err() != nil || len(ids) != 2 || index < 0 || index > 1 {
		return rejected
	}
	value, err := engine.InspectContainer(ctx, ids[index])
	if err != nil || ctx.Err() != nil {
		return rejected
	}
	return validateContainer(value, c, public, ids[index], index)
}
