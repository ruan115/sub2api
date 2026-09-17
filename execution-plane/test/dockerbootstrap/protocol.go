// This executable is a trusted, synthetic host laboratory controller. Docker
// socket access is daemon-administrator access, not a UID isolation boundary.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

const baseImage = "sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807"
const ownerLabel = "org.sub2api.isthmus.lab-run"
const nodeID = "node-live"

var rejected = errors.New("docker bootstrap probe rejected")
var ownerPattern = regexp.MustCompile(`^isthmus-s1b-[a-z0-9-]+$`)
var workerPattern = regexp.MustCompile(`^/var/tmp/isthmus-s1b\.[A-Za-z0-9]{8}/live-bin/worker$`)
var cidPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type initialInput struct {
	Owner      string `json:"owner"`
	ImageID    string `json:"image_id"`
	WorkerPath string `json:"worker_path"`
}

type containerInput struct {
	ContainerIDs []string `json:"container_ids"`
}

type publicConfig struct {
	TrustSHA256     string `json:"trust_sha256"`
	TicketPublicKey string `json:"ticket_public_key"`
}

type summary struct {
	ActualDockerExec        bool `json:"actual_docker_exec"`
	AuthenticatedEnrollment bool `json:"authenticated_enrollment"`
	IndependentKeys         bool `json:"independent_keys"`
	SameKeyLeafStable       bool `json:"same_key_leaf_stable"`
	CrossInstallRejected    bool `json:"cross_install_rejected"`
	WrongCARejected         bool `json:"wrong_ca_rejected"`
	LeaseRetryRejected      bool `json:"lease_retry_rejected"`
	PublicInstallRetry      bool `json:"public_install_retry"`
	TCPReady                bool `json:"tcp_ready"`
	CrossContainerMTLS      bool `json:"cross_container_mtls"`
	ProductionReady         bool `json:"production_ready"`
}

func (c initialInput) validate() error {
	if len(c.Owner) > 80 || !ownerPattern.MatchString(c.Owner) || c.ImageID != baseImage || !workerPattern.MatchString(c.WorkerPath) || filepath.Clean(c.WorkerPath) != c.WorkerPath {
		return rejected
	}
	root := filepath.Base(filepath.Dir(filepath.Dir(c.WorkerPath)))
	if c.Owner != strings.ToLower(strings.Replace(root, ".", "-", 1)) {
		return rejected
	}
	return nil
}

func (c containerInput) validate() error {
	if len(c.ContainerIDs) != 2 || !cidPattern.MatchString(c.ContainerIDs[0]) || !cidPattern.MatchString(c.ContainerIDs[1]) || c.ContainerIDs[0] == c.ContainerIDs[1] {
		return rejected
	}
	return nil
}

// Exact, duplicate-free top-level fields, bounded before JSON allocation. No
// decoder error (which might contain input) is returned to stdout/stderr.
func readLine(scanner *bufio.Scanner, target any, fields ...string) error {
	if !scanner.Scan() || len(scanner.Bytes()) > 4096 {
		return rejected
	}
	line := scanner.Bytes()
	d := json.NewDecoder(bytes.NewReader(line))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return rejected
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] {
			return rejected
		}
		allowed := false
		for _, field := range fields {
			allowed = allowed || field == key
		}
		if !allowed {
			return rejected
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return rejected
		}
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') || len(seen) != len(fields) {
		return rejected
	}
	if _, err := d.Token(); err != io.EOF {
		return rejected
	}
	if json.Unmarshal(line, target) != nil {
		return rejected
	}
	return nil
}

func inputScanner(reader io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 4097), 4097)
	return s
}
