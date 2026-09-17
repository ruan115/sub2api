package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func TestCommandHasOnlyPublicOutputAndNoImplicitImport(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chmod(dir, 0700)
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot", NodeID: "node", Epoch: 1, Generation: 1}
	data, _ := json.Marshal(binding)
	for _, mode := range []string{"init", "show", "request"} {
		var out bytes.Buffer
		if err := run([]string{mode, dir}, bytes.NewReader(data), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "private_key") {
			t.Fatal("private output")
		}
		var result struct {
			Public runtimeidentity.Public `json:"public"`
			CSR    string                 `json:"csr_pem"`
		}
		if json.Unmarshal(out.Bytes(), &result) != nil || result.Public.Binding != binding {
			t.Fatal("bad public response")
		}
		if mode == "request" {
			if _, err := runtimeidentity.ValidateCSR(binding, []byte(result.CSR)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, body := range []string{`{"private_key":"do-not-echo"}`, `{"epoch":2,` + string(data[1:]), string(data) + " {}", strings.Repeat("x", 4097), "invalid"} {
		var out bytes.Buffer
		if err := run([]string{"init", dir}, strings.NewReader(body), &out); err != errCommand || out.Len() != 0 {
			t.Fatal("unsafe command accepted or echoed")
		}
	}
	for _, args := range [][]string{{}, {"import", dir}, {"rotate", dir}, {"request", dir, "secret"}} {
		var out bytes.Buffer
		if run(args, bytes.NewReader(data), &out) != errCommand || out.Len() != 0 {
			t.Fatal("unknown command accepted")
		}
	}
}
