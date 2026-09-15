package upstreamusage

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const startEvent = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0,\"cache_creation_input_tokens\":7,\"cache_read_input_tokens\":4,\"cache_creation\":{\"ephemeral_5m_input_tokens\":2,\"ephemeral_1h_input_tokens\":5}}}}\n\n"
const stopEvent = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func event(kind, extra string) string {
	return "event: " + kind + "\ndata: {\"type\":\"" + kind + "\"" + extra + "}\n\n"
}

func completeFixture() string {
	return startEvent +
		event("content_block_start", `,"index":0,"content_block":{"type":"text","text":""}`) +
		event("content_block_delta", `,"index":0,"delta":{"type":"text_delta","text":"合成 🧪 content"},"usage":{"input_tokens":99999}`) +
		event("content_block_stop", `,"index":0`) +
		event("message_delta", `,"usage":{"input_tokens":2,"output_tokens":33,"cache_creation":{"ephemeral_5m_input_tokens":1},"output_tokens_details":{"thinking_tokens":21}}`) +
		event("message_delta", `,"usage":{"output_tokens":20,"output_tokens_details":{"thinking_tokens":11}}`) + stopEvent
}

func TestSSEArbitraryChunkingAndCumulativeMaximum(t *testing.T) {
	fixture := completeFixture()
	for _, size := range []int{1, 2, 3, 7, 128, len(fixture)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := NewSSE()
			for offset := 0; offset < len(fixture); offset += size {
				s.Feed([]byte(fixture[offset:min(offset+size, len(fixture))]))
				assertUsage(t, s.UsageJSON(), "")
			}
			if err := s.Finish(); err != nil {
				t.Fatal(err)
			}
			assertUsage(t, s.UsageJSON(), `{"input_tokens":3,"output_tokens":33,"cache_creation_input_tokens":7,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":5},"output_tokens_details":{"thinking_tokens":21}}`)
			first := s.UsageJSON()
			first[0] = 'x'
			if s.Finish() != nil || s.UsageJSON()[0] != '{' {
				t.Fatal("Finish must be stable and results independently owned")
			}
		})
	}
}

func TestSSEFramingBOMCommentsCRLFAndMultiline(t *testing.T) {
	stream := "\ufeff: synthetic comment\nevent: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_start\nid: synthetic\nretry: 1000\ndata: {\"type\":\"message_start\",\ndata: \"message\":{\"usage\":{\"input_tokens\":0}}}\n\n" + stopEvent + ": trailing comment\n\n"
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			s := NewSSE()
			for _, b := range []byte(strings.ReplaceAll(stream, "\n", ending)) {
				s.Feed([]byte{b})
			}
			if s.Finish() != nil {
				t.Fatal("valid SSE framing rejected")
			}
			assertUsage(t, s.UsageJSON(), `{"input_tokens":0}`)
		})
	}
}

func TestSSENoInventedUsage(t *testing.T) {
	s := NewSSE()
	s.Feed([]byte(event("message_start", `,"message":{}`) + event("message_delta", `,"usage":{"output_tokens":-1,"input_tokens":"9"}`) + stopEvent))
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, s.UsageJSON(), "")
}

func TestSSEDataOnlyEventsAndMultipleContentBlocks(t *testing.T) {
	stream := event("message_start", `,"message":{"usage":{"input_tokens":1}}`)
	for index := range 2 {
		stream += event("content_block_start", fmt.Sprintf(`,"index":%d,"content_block":{}`, index))
		stream += event("future_extension", `,"usage":{"input_tokens":99}`)
		stream += event("content_block_delta", fmt.Sprintf(`,"index":%d,"delta":{"text":"synthetic"}`, index))
		stream += event("content_block_stop", fmt.Sprintf(`,"index":%d`, index))
	}
	stream += event("message_delta", `,"usage":{"output_tokens":2}`) + stopEvent
	var dataOnly strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "event:") {
			dataOnly.WriteString(line + "\n")
		}
	}
	s := NewSSE()
	s.Feed([]byte(dataOnly.String()))
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, s.UsageJSON(), `{"input_tokens":1,"output_tokens":2}`)
}

func TestSSEFutureEventsDoNotContributeUsageOrCompletion(t *testing.T) {
	future := event("future_extension_v2", `,"usage":{"input_tokens":99999},"payload":{"usage":{"output_tokens":888}}`)
	s := NewSSE()
	s.Feed([]byte(startEvent + future + stopEvent + event("ping", ``)))
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, s.UsageJSON(), `{"input_tokens":3,"output_tokens":0,"cache_creation_input_tokens":7,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":5}}`)
	for _, stream := range []string{future + startEvent + stopEvent, startEvent + future, startEvent + stopEvent + future} {
		s := NewSSE()
		s.Feed([]byte(stream))
		if !errors.Is(s.Finish(), ErrInvalid) || s.UsageJSON() != nil {
			t.Fatal("extension must not establish or revive completion")
		}
	}
}

func TestSSEInvalidAndTruncatedStreamsNeverComplete(t *testing.T) {
	blockStart := event("content_block_start", `,"index":0,"content_block":{}`)
	for name, stream := range map[string]string{
		"empty":                "",
		"no stop":              startEvent,
		"stop without start":   stopEvent,
		"duplicate start":      startEvent + startEvent + stopEvent,
		"duplicate stop":       startEvent + stopEvent + stopEvent,
		"delta before start":   event("message_delta", `,"usage":{"output_tokens":9}`) + startEvent + stopEvent,
		"delta after stop":     startEvent + stopEvent + event("message_delta", `,"usage":{"output_tokens":9}`),
		"error":                startEvent + event("error", `,"error":{"message":"synthetic_secret"}`) + stopEvent,
		"error event only":     startEvent + "event: error\n\n" + stopEvent,
		"error after stop":     startEvent + stopEvent + event("error", ``),
		"missing message":      event("message_start", ``) + stopEvent,
		"bad message":          event("message_start", `,"message":[]`) + stopEvent,
		"event mismatch":       startEvent + "event: ping\ndata: {\"type\":\"message_stop\"}\n\n",
		"duplicate event":      startEvent + "event: ping\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"missing type":         startEvent + "data: {\"usage\":{\"output_tokens\":9}}\n\n" + stopEvent,
		"bad type":             startEvent + "data: {\"type\":{}}\n\n" + stopEvent,
		"invalid type string":  startEvent + event("unexpected secret value", ``) + stopEvent,
		"duplicate usage key":  startEvent + event("message_delta", `,"usage":{"output_tokens":1,"output_tokens":9}`) + stopEvent,
		"duplicate type":       startEvent + "data: {\"type\":\"ping\",\"type\":\"message_stop\"}\n\n",
		"bad JSON":             startEvent + "data: {malformed synthetic_secret}\n\n" + stopEvent,
		"invalid UTF8":         startEvent + "data: {\"type\":\"ping\",\"text\":\"\xff\"}\n\n" + stopEvent,
		"unterminated stop":    startEvent + strings.TrimSuffix(stopEvent, "\n"),
		"unterminated line":    startEvent + stopEvent + "data: partial",
		"open block":           startEvent + blockStart + stopEvent,
		"unmatched block stop": startEvent + event("content_block_stop", `,"index":0`) + stopEvent,
		"unmatched delta":      startEvent + event("content_block_delta", `,"index":0,"delta":{}`) + stopEvent,
		"block index gap":      startEvent + event("content_block_start", `,"index":1,"content_block":{}`) + stopEvent,
		"block missing index":  startEvent + event("content_block_start", `,"content_block":{}`) + stopEvent,
		"block bad index":      startEvent + event("content_block_start", `,"index":0.5,"content_block":{}`) + stopEvent,
		"delta while block":    startEvent + blockStart + event("message_delta", `,"usage":{"output_tokens":1}`) + stopEvent,
		"block after delta":    startEvent + event("message_delta", ``) + blockStart + stopEvent,
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSSE()
			s.Feed([]byte(stream))
			if s.Finish() != ErrInvalid || s.Finish().Error() != "upstream_usage_invalid" {
				t.Fatal("expected fixed invalid error")
			}
			assertUsage(t, s.UsageJSON(), "")
		})
	}
}

func TestSSEOversizeAndDepthBounds(t *testing.T) {
	for name, stream := range map[string]string{
		"line":    startEvent + "data: " + strings.Repeat("x", maxSSEBytes+1),
		"comment": startEvent + ":" + strings.Repeat("x", maxSSEBytes+1),
		"event":   startEvent + strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", 1025) + "\n",
		"depth":   startEvent + event("future_extension", `,"value":`+strings.Repeat("[", maxJSONDepth+1)+"0"+strings.Repeat("]", maxJSONDepth+1)),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSSE()
			s.Feed([]byte(stream))
			if !s.invalid || len(s.line) != 0 || len(s.data) != 0 {
				t.Fatal("invalid observer must release retained input")
			}
			s.Feed([]byte(stopEvent))
			if s.Finish() != ErrInvalid || s.UsageJSON() != nil {
				t.Fatal("oversize/deep event became trusted")
			}
		})
	}
}

func TestSSECumulativeStreamMayExceedTwoMiB(t *testing.T) {
	s := NewSSE()
	s.Feed([]byte(startEvent + event("content_block_start", `,"index":0,"content_block":{}`)))
	chunk := []byte(event("content_block_delta", `,"index":0,"delta":{"text":"`+strings.Repeat("x", 4096)+`"}`))
	original := append([]byte(nil), chunk...)
	for range 1024 {
		s.Feed(chunk)
	}
	s.Feed([]byte(event("content_block_stop", `,"index":0`) + stopEvent))
	if s.Finish() != nil || s.UsageJSON() == nil || !bytes.Equal(chunk, original) {
		t.Fatal("bounded observer must not cap or mutate the entire stream")
	}
}

func TestSSEFeedAfterFinishInvalidatesUsage(t *testing.T) {
	s := NewSSE()
	s.Feed([]byte(startEvent + stopEvent))
	if s.Finish() != nil {
		t.Fatal("expected complete stream")
	}
	s.Feed(nil)
	if s.UsageJSON() == nil {
		t.Fatal("empty feed should be harmless")
	}
	s.Feed([]byte("data: another event\n\n"))
	if s.Finish() != ErrInvalid || s.UsageJSON() != nil {
		t.Fatal("bytes after finalized EOF cannot revive completion")
	}
}

func FuzzSSEChunkBoundaries(f *testing.F) {
	f.Add([]byte(completeFixture()), uint8(1))
	f.Add([]byte(startEvent), uint8(17))
	f.Add([]byte("data: {\"type\":\"error\"}\n\n"), uint8(3))
	f.Fuzz(func(t *testing.T, body []byte, width uint8) {
		one, many := NewSSE(), NewSSE()
		one.Feed(body)
		for i, size := 0, int(width)+1; i < len(body); i += size {
			many.Feed(body[i:min(i+size, len(body))])
		}
		if one.Finish() != many.Finish() || !bytes.Equal(one.UsageJSON(), many.UsageJSON()) {
			t.Fatal("semantic result depends on transport chunk boundaries")
		}
	})
}
