// instance-identity is an explicit Linux instance-local identity operation.
// It has no listener, signing authority, private export, import or rotate mode.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var errCommand = errors.New("instance identity operation rejected")

func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 2 || (args[0] != "init" && args[0] != "show" && args[0] != "request") {
		return errCommand
	}
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(data) == 0 || len(data) > 4096 {
		return errCommand
	}
	var binding runtimeidentity.Binding
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errCommand
	}
	fields := make(map[string]json.RawMessage, 5)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return errCommand
		}
		if _, exists := fields[name]; exists {
			return errCommand
		}
		switch name {
		case "account_hash", "slot_id", "node_id", "epoch", "generation":
		default:
			return errCommand
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return errCommand
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 5 {
		return errCommand
	}
	canonical, err := json.Marshal(fields)
	if err != nil || json.Unmarshal(canonical, &binding) != nil || binding.Validate() != nil {
		return errCommand
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errCommand
	}
	identity, err := runtimeidentity.Open(args[1], binding, args[0] == "init")
	if err != nil {
		return errCommand
	}
	result := struct {
		Public runtimeidentity.Public `json:"public"`
		CSR    string                 `json:"csr_pem,omitempty"`
	}{Public: identity.Public()}
	if args[0] == "request" {
		csr, err := identity.CSR()
		if err != nil {
			return errCommand
		}
		result.CSR = string(csr)
	}
	if json.NewEncoder(output).Encode(result) != nil {
		return errCommand
	}
	return nil
}

func main() {
	if runtime.GOOS != "linux" || os.Geteuid() != 1000 || run(os.Args[1:], os.Stdin, os.Stdout) != nil {
		fmt.Fprintln(os.Stderr, errCommand)
		os.Exit(1)
	}
}
