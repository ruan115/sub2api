package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
)

func containerFixture(config initialInput, public publicConfig, cid string, index int) docker.Container {
	c := docker.Container{ID: cid, Name: "/" + config.Owner + "-live-" + []string{"a", "b"}[index], Image: baseImage}
	c.Config.Image, c.Config.User, c.Config.Hostname = baseImage, "1000:1000", strings.TrimPrefix(c.Name, "/")
	c.Config.Labels = map[string]string{ownerLabel: config.Owner}
	for k, v := range expectedEnvironment(index, public) {
		c.Config.Env = append(c.Config.Env, k+"="+v)
	}
	c.HostConfig = docker.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, IpcMode: "private", CgroupnsMode: "private", Memory: 1 << 30, NanoCPUs: 1_000_000_000, PidsLimit: 128, RestartPolicy: docker.RestartPolicy{Name: "no"}, Binds: []string{config.WorkerPath + ":/worker:ro"}, Tmpfs: map[string]string{}}
	for k, v := range tmpfs {
		c.HostConfig.Tmpfs[k] = v
	}
	c.NetworkSettings.Networks = map[string]docker.NetworkEndpoint{"none": {}}
	c.State.Running = true
	raw, _ := json.Marshal(map[string]any{"Mounts": []map[string]any{{"Type": "bind", "Source": config.WorkerPath, "Destination": "/worker", "RW": false, "Propagation": "rprivate"}}})
	_ = json.Unmarshal(raw, &c)
	return c
}

func TestContainerPolicyRejectsDriftBeforeAnyOperation(t *testing.T) {
	cid := strings.Repeat("a", 64)
	changes := map[string]func(*docker.Container){
		"id": func(c *docker.Container) { c.ID = strings.Repeat("b", 64) }, "owner": func(c *docker.Container) { c.Config.Labels[ownerLabel] = "other" },
		"name": func(c *docker.Container) { c.Name = "/other" }, "hostname": func(c *docker.Container) { c.Config.Hostname = "other" }, "image": func(c *docker.Container) { c.Image = "other" },
		"root": func(c *docker.Container) { c.Config.User = "0:0" }, "stopped": func(c *docker.Container) { c.State.Running = false }, "rootfs": func(c *docker.Container) { c.HostConfig.ReadonlyRootfs = false },
		"privileged": func(c *docker.Container) { c.HostConfig.Privileged = true }, "network": func(c *docker.Container) { c.HostConfig.NetworkMode = "host" },
		"ports": func(c *docker.Container) {
			c.HostConfig.PortBindings = map[string][]docker.PortBinding{"80/tcp": {{HostPort: "80"}}}
		},
		"cpu": func(c *docker.Container) { c.HostConfig.NanoCPUs++ }, "memory": func(c *docker.Container) { c.HostConfig.Memory++ }, "pids": func(c *docker.Container) { c.HostConfig.PidsLimit++ },
		"cap": func(c *docker.Container) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} }, "capdrop": func(c *docker.Container) { c.HostConfig.CapDrop = nil }, "nnp": func(c *docker.Container) { c.HostConfig.SecurityOpt = nil },
		"namespace": func(c *docker.Container) { c.HostConfig.PidMode = "host" }, "mount": func(c *docker.Container) {
			c.HostConfig.Binds = append(c.HostConfig.Binds, "/var/run/docker.sock:/docker.sock:ro")
		},
		"mount-rw": func(c *docker.Container) { c.Mounts[0].RW = true }, "mount-source": func(c *docker.Container) { c.Mounts[0].Source = "/other" }, "mount-missing": func(c *docker.Container) { c.Mounts = nil },
		"tmpfs": func(c *docker.Container) { c.HostConfig.Tmpfs["/run"] = "rw" }, "env-extra": func(c *docker.Container) { c.Config.Env = append(c.Config.Env, "EXTRA=synthetic") },
		"env-duplicate": func(c *docker.Container) { c.Config.Env = append(c.Config.Env, c.Config.Env[0]) }, "env-other-instance": func(c *docker.Container) {
			c.Config.Env = nil
			for k, v := range expectedEnvironment(1, testPublic()) {
				c.Config.Env = append(c.Config.Env, k+"="+v)
			}
		},
		"endpoint": func(c *docker.Container) {
			c.NetworkSettings.Networks["none"] = docker.NetworkEndpoint{IPAddress: "127.0.0.1"}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c := containerFixture(testConfig(), testPublic(), cid, 0)
			if validateContainer(c, testConfig(), testPublic(), cid, 0) != nil {
				t.Fatal("valid fixture rejected")
			}
			change(&c)
			if validateContainer(c, testConfig(), testPublic(), cid, 0) == nil {
				t.Fatal("drift accepted")
			}
		})
	}
}
