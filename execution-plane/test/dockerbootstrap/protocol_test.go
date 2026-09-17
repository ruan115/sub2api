package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testConfig() initialInput {
	return initialInput{Owner: "isthmus-s1b-abcd1234", ImageID: baseImage, WorkerPath: "/var/tmp/isthmus-s1b.AbCd1234/live-bin/worker"}
}
func testPublic() publicConfig {
	return publicConfig{TrustSHA256: strings.Repeat("a", 64), TicketPublicKey: strings.Repeat("a", 43)}
}

func TestInputContractRejectsAmbiguousOrUnboundedJSON(t *testing.T) {
	valid, _ := json.Marshal(testConfig())
	for name, raw := range map[string]string{
		"valid": string(valid), "duplicate": strings.Replace(string(valid), `"owner":`, `"owner":"other","owner":`, 1),
		"unknown": strings.TrimSuffix(string(valid), "}") + `,"secret":"not-output"}`,
		"missing": `{"owner":"x"}`, "trailing": string(valid) + ` {}`, "null": `null`, "array": `[]`, "large": strings.Repeat(" ", 4097) + string(valid),
		"wrongtype": strings.Replace(string(valid), `"image_id":"`+baseImage+`"`, `"image_id":true`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var c initialInput
			err := readLine(inputScanner(strings.NewReader(raw+"\n")), &c, "owner", "image_id", "worker_path")
			if (err == nil) != (name == "valid") {
				t.Fatal("JSON boundary differs")
			}
		})
	}
}

func TestInputPathsAndIDsAreFixed(t *testing.T) {
	if testConfig().validate() != nil {
		t.Fatal("valid input rejected")
	}
	for _, change := range []func(*initialInput){
		func(c *initialInput) { c.Owner = "another" }, func(c *initialInput) { c.Owner = "isthmus-s1b-other" }, func(c *initialInput) { c.ImageID = "sha256:" + strings.Repeat("a", 64) },
		func(c *initialInput) { c.WorkerPath = "/var/tmp/isthmus-s1b.AbCd1234/live-bin/../worker" }, func(c *initialInput) { c.WorkerPath = "/tmp/isthmus-s1b.AbCd1234/live-bin/worker" },
		func(c *initialInput) { c.WorkerPath = "/var/tmp/isthmus-s1b.AbCd12345/live-bin/worker" }, func(c *initialInput) { c.WorkerPath += ";id" },
	} {
		c := testConfig()
		change(&c)
		if c.validate() == nil {
			t.Fatal("unsafe path or owner accepted")
		}
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, ids := range [][]string{nil, {a}, {a, a}, {strings.ToUpper(a), b}, {a, b, "other"}, {"", b}} {
		if (containerInput{ids}).validate() == nil {
			t.Fatal("invalid ID set accepted")
		}
	}
	if (containerInput{[]string{a, b}}).validate() != nil {
		t.Fatal("valid ID set rejected")
	}
}

func TestInvalidInitialInputProducesNoPublicConfig(t *testing.T) {
	var out bytes.Buffer
	if stage := run(strings.NewReader(`{"owner":"secret-not-echoed"}`+"\n"), &out); stage != "input" || out.Len() != 0 {
		t.Fatal("invalid first line emitted data")
	}
}
