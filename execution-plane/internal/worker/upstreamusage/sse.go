package upstreamusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"
)

// ErrInvalid deliberately carries no upstream text. It covers malformed,
// oversized, truncated, error and semantically incomplete event streams.
var ErrInvalid = errors.New("upstream_usage_invalid")

const maxSSEBytes = 1 << 20 // Both each line and each event's joined data.

var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// SSE is a bounded, single-owner observer. It is not safe for concurrent use.
// The caller must forward original bytes separately and call Finish at EOF.
type SSE struct {
	line      []byte
	data      []byte
	event     string
	eventSeen bool
	dataSeen  bool
	firstLine bool
	skipLF    bool
	invalid   bool
	finished  bool
	started   bool
	stopped   bool
	deltaSeen bool
	blockOpen bool
	nextBlock int64
	usage     counters
}

func NewSSE() *SSE { return &SSE{firstLine: true} }

// Feed accepts arbitrary byte boundaries, including split UTF-8 and CRLF. On an
// invalid or oversized event it drops retained input, becomes untrusted and
// ignores further bytes; this does not alter the caller's forwarding stream.
func (s *SSE) Feed(chunk []byte) {
	if len(chunk) == 0 || s.invalid {
		return
	}
	if s.finished {
		s.fail()
		return
	}
	for _, b := range chunk {
		if s.skipLF {
			s.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			s.consumeLine()
			s.line = s.line[:0]
			s.skipLF = b == '\r'
			if s.invalid {
				return
			}
			continue
		}
		if len(s.line) >= maxSSEBytes {
			s.fail()
			return
		}
		s.line = append(s.line, b)
	}
}

func (s *SSE) fail() {
	s.invalid = true
	s.line, s.data, s.event = nil, nil, ""
	s.usage = counters{}
}

func (s *SSE) consumeLine() {
	line := s.line
	if s.firstLine {
		s.firstLine = false
		line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	}
	if !utf8.Valid(line) {
		s.fail()
		return
	}
	if len(line) == 0 {
		if s.event == "error" {
			s.fail()
			return
		}
		if s.dataSeen {
			s.dispatch()
		}
		s.data = s.data[:0]
		s.event, s.eventSeen, s.dataSeen = "", false, false
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "event":
		if s.eventSeen {
			s.fail()
			return
		}
		s.eventSeen, s.event = true, string(value)
	case "data":
		extra := len(value)
		if s.dataSeen {
			extra++
		}
		if extra > maxSSEBytes-len(s.data) {
			s.fail()
			return
		}
		if s.dataSeen {
			s.data = append(s.data, '\n')
		}
		s.data, s.dataSeen = append(s.data, value...), true
	}
}

func (s *SSE) dispatch() {
	doc, ok := decodeObject(s.data)
	if !ok {
		s.fail()
		return
	}
	kind, ok := doc["type"].(string)
	if !ok || !eventTypePattern.MatchString(kind) || (s.event != "" && s.event != kind) {
		s.fail()
		return
	}
	if kind == "error" {
		s.fail()
		return
	}
	if kind == "ping" {
		return
	}
	if s.stopped {
		s.fail()
		return
	}
	switch kind {
	case "message_start":
		message, isObject := doc["message"].(map[string]any)
		if s.started || !isObject {
			s.fail()
			return
		}
		s.started = true
		s.usage.observe(message["usage"])
	case "message_delta":
		if !s.started || s.blockOpen {
			s.fail()
			return
		}
		s.deltaSeen = true
		s.usage.observe(doc["usage"])
	case "message_stop":
		if !s.started || s.blockOpen {
			s.fail()
			return
		}
		s.stopped = true
	case "content_block_start", "content_block_delta", "content_block_stop":
		index, isNumber := doc["index"].(json.Number)
		n, err := index.Int64()
		if !s.started || s.deltaSeen || !isNumber || err != nil || n < 0 || n != s.nextBlock {
			s.fail()
			return
		}
		switch kind {
		case "content_block_start":
			if _, ok := doc["content_block"].(map[string]any); !ok || s.blockOpen {
				s.fail()
				return
			}
			s.blockOpen = true
		case "content_block_delta":
			if _, ok := doc["delta"].(map[string]any); !ok || !s.blockOpen {
				s.fail()
			}
		case "content_block_stop":
			if !s.blockOpen {
				s.fail()
				return
			}
			s.blockOpen = false
			s.nextBlock++
		}
	default:
		// Anthropic may introduce new event types. A well-formed extension in
		// an active message is forwarded by the caller but cannot contribute
		// usage, open/close known content blocks, or establish completion.
		if !s.started {
			s.fail()
		}
	}
}

// Finish succeeds only after a dispatched message_stop, with no pending partial
// line/event or invalid sequence. Repeated calls are stable. Partial usage is
// intentionally withheld: it must not be attached to a successful completion.
func (s *SSE) Finish() error {
	s.finished = true
	if s.invalid || !s.stopped || len(s.line) != 0 || s.dataSeen || s.eventSeen {
		s.fail()
		return ErrInvalid
	}
	return nil
}

// UsageJSON is available only after Finish succeeds. Each call owns its returned
// bytes. Absent valid counters return nil rather than zero-valued accounting.
func (s *SSE) UsageJSON() []byte {
	if !s.finished || s.invalid || !s.stopped {
		return nil
	}
	return s.usage.json()
}
