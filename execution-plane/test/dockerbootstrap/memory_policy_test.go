package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
)

// The administrator must not rely solely on the Python outer inspector for
// this projected field. This is Engine metadata evidence, not cgroup proof.
func TestContainerPolicyRejectsAbsentOrNullSwapEvidence(t *testing.T) {
	for _, missing := range []bool{true, false} {
		name := "null"
		if missing {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			cid := strings.Repeat("a", 64)
			positive := containerFixture(testConfig(), testPublic(), cid, 0)
			if validateContainer(positive, testConfig(), testPublic(), cid, 0) != nil {
				t.Fatal("valid no-swap fixture rejected")
			}
			raw, err := json.Marshal(positive)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			var host map[string]json.RawMessage
			if err := json.Unmarshal(document["HostConfig"], &host); err != nil {
				t.Fatal(err)
			}
			if missing {
				delete(host, "MemorySwap")
			} else {
				host["MemorySwap"] = json.RawMessage("null")
			}
			document["HostConfig"], err = json.Marshal(host)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var decoded docker.Container
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.HostConfig.MemorySwap != 0 {
				t.Fatal("missing evidence did not decode to unsafe default")
			}
			if validateContainer(decoded, testConfig(), testPublic(), cid, 0) == nil {
				t.Fatal("missing no-swap evidence accepted")
			}
		})
	}
}
