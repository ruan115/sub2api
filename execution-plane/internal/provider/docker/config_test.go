package docker

import (
	"strings"
	"testing"
)

func TestConfigRejectsDisabledAndEmptySandboxProfiles(t *testing.T) {
	for _, kind := range []string{"seccomp", "apparmor"} {
		for _, profile := range []string{"", " ", "\t", "builtin\n", " custom ", "unconfined", "UNCONFINED", "custom\x00profile", "custom\nprofile", string([]byte{0xff})} {
			t.Run(kind+"/"+strings.ReplaceAll(profile, "\n", "newline"), func(t *testing.T) {
				config := DefaultConfig()
				if kind == "seccomp" {
					config.AllowedSeccompProfiles = append(config.AllowedSeccompProfiles, profile)
				} else {
					config.AllowedAppArmorProfiles = append(config.AllowedAppArmorProfiles, profile)
				}
				engine := &fakeEngine{}
				if err := config.Validate(); err == nil {
					t.Fatal("unsafe sandbox profile passed configuration validation")
				}
				if provider, err := New(config, engine); err == nil || provider != nil || len(engine.calls) != 0 {
					t.Fatal("unsafe sandbox profile reached an operational provider")
				}
			})
		}
	}
}

func TestProviderRetainsValidatedProfilePolicy(t *testing.T) {
	config := DefaultConfig()
	provider, err := New(config, &fakeEngine{})
	if err != nil {
		t.Fatal(err)
	}
	config.AllowedSeccompProfiles[0] = "unconfined"
	config.AllowedAppArmorProfiles[0] = "unconfined"
	if provider.config.AllowedSeccompProfiles[0] != "builtin" || provider.config.AllowedAppArmorProfiles[0] != "docker-default" {
		t.Fatal("caller mutation replaced the validated sandbox policy")
	}
}

func TestConfigAllowsBuiltinAndCustomSandboxProfiles(t *testing.T) {
	config := DefaultConfig()
	config.AllowedSeccompProfiles = []string{"builtin", "/etc/docker/seccomp/worker.json", "strict-seccomp"}
	config.AllowedAppArmorProfiles = []string{"docker-default", "execution-worker", "strict-apparmor"}
	if _, err := New(config, &fakeEngine{}); err != nil {
		t.Fatal("valid builtin/custom sandbox profiles were rejected")
	}
}
