package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testSink struct {
	headers func(context.Context, int, map[string]string) error
	chunk   func(context.Context, []byte) error
}

func (s testSink) Headers(ctx context.Context, status int, headers map[string]string) error {
	if s.headers != nil {
		return s.headers(ctx, status, headers)
	}
	return nil
}

func (s testSink) Chunk(ctx context.Context, data []byte) error {
	if s.chunk != nil {
		return s.chunk(ctx, data)
	}
	return nil
}

type trackedBody struct {
	reader io.Reader
	closer io.Closer
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}

func (b *trackedBody) Close() error {
	b.closes.Add(1)
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

func responseWith(body *trackedBody, code int, contentType string) *http.Response {
	return &http.Response{
		StatusCode: code, Body: body, ContentLength: -1,
		Header: http.Header{"Content-Type": {contentType}, "X-Request-Id": {"req-synthetic"}},
	}
}

func assertClosed(t *testing.T, body *trackedBody) {
	t.Helper()
	if count := body.closes.Load(); count != 1 {
		t.Fatalf("body closed %d times, want exactly once", count)
	}
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local fixture")
		var zero T
		return zero
	}
}

func TestRelayStreamsFirstChunkBeforeResponseCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &trackedBody{reader: reader, closer: reader}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	chunks := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		result, err := Relay(ctx, responseWith(body, 200, "text/event-stream; charset=utf-8"), testSink{
			chunk: func(_ context.Context, data []byte) error { chunks <- string(data); return nil },
		}, nil)
		if err == nil && (!result.Streaming || result.RequestID != "req-synthetic") {
			err = errors.New("unexpected relay result")
		}
		done <- err
	}()
	first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	if _, err := io.WriteString(writer, first); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, chunks); got != first {
		t.Fatalf("first chunk = %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("relay finished before upstream EOF: %v", err)
	default:
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	assertClosed(t, body)
}

func TestMalformedMediaTypeRejectsBeforeReadingOrDelivery(t *testing.T) {
	for _, unary := range []bool{false, true} {
		body := &trackedBody{reader: strings.NewReader("synthetic")}
		response := responseWith(body, 200, "text/event-stream; broken")
		if IsStreaming(response) {
			t.Fatal("malformed media type classified as SSE")
		}
		var err error
		delivered := false
		if unary {
			_, err = ReadUnary(context.Background(), response)
		} else {
			_, err = Relay(context.Background(), response, testSink{
				headers: func(context.Context, int, map[string]string) error { delivered = true; return nil },
				chunk:   func(context.Context, []byte) error { delivered = true; return nil },
			}, nil)
		}
		if err != ErrResponse || delivered || body.reads.Load() != 0 {
			t.Fatal("malformed media type consumed")
		}
		assertClosed(t, body)
	}
}

func TestRelayPreservesBytesHeadersAndObserverOrder(t *testing.T) {
	input := []byte(strings.Repeat("data: 中文\r\n\r\n", ChunkBytes/4) + "event: message_stop\ndata: {}\n\n")
	body := &trackedBody{reader: bytes.NewReader(input)}
	response := responseWith(body, 201, "Text/Event-Stream; charset=UTF-8")
	response.Header.Set("Authorization", "secret-must-not-be-forwarded")
	response.Header.Set("Set-Cookie", "secret-cookie")
	response.Header.Set("Content-Encoding", " identity ")
	var delivered, observed []byte
	var events []string
	result, err := Relay(context.Background(), response, testSink{
		headers: func(_ context.Context, code int, headers map[string]string) error {
			if code != 201 || !reflect.DeepEqual(headers, map[string]string{"content-type": "Text/Event-Stream; charset=UTF-8", "x-request-id": "req-synthetic"}) {
				t.Fatalf("unexpected headers: code=%d keys=%v", code, headers)
			}
			events = append(events, "headers")
			return nil
		},
		chunk: func(_ context.Context, data []byte) error {
			if len(data) == 0 || len(data) > ChunkBytes {
				t.Fatalf("invalid chunk length %d", len(data))
			}
			delivered = append(delivered, data...)
			events = append(events, "chunk")
			return nil
		},
	}, func(data []byte) {
		if events[len(events)-1] != "chunk" {
			t.Fatal("observer ran before delivery")
		}
		observed = append(observed, data...)
		events = append(events, "observe")
	})
	if err != nil || !result.Streaming || !bytes.Equal(input, delivered) || !bytes.Equal(input, observed) {
		t.Fatalf("byte preservation failed: result=%+v err=%v delivered=%d observed=%d", result, err, len(delivered), len(observed))
	}
	if events[0] != "headers" {
		t.Fatal("body preceded headers")
	}
	assertClosed(t, body)
}

type repeatedReader struct{}

func (repeatedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestRelayDoesNotApplyUnaryLimitToCumulativeStream(t *testing.T) {
	const total = 3*MaxUnaryBytes + 17
	body := &trackedBody{reader: io.LimitReader(repeatedReader{}, total)}
	var delivered, observed, maximum int
	_, err := Relay(context.Background(), responseWith(body, 200, "text/event-stream"), testSink{
		chunk: func(_ context.Context, data []byte) error {
			delivered += len(data)
			maximum = max(maximum, len(data))
			return nil
		},
	}, func(data []byte) { observed += len(data) })
	if err != nil || delivered != total || observed != total || maximum != ChunkBytes {
		t.Fatalf("stream sizes: delivered=%d observed=%d max=%d err=%v", delivered, observed, maximum, err)
	}
	assertClosed(t, body)
}

func TestRelayBackpressureDoesNotReadAhead(t *testing.T) {
	body := &trackedBody{reader: io.LimitReader(repeatedReader{}, 2*ChunkBytes)}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var first sync.Once
	go func() {
		_, err := Relay(ctx, responseWith(body, 200, "text/event-stream"), testSink{
			chunk: func(ctx context.Context, _ []byte) error {
				first.Do(func() { entered <- struct{}{} })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}, nil)
		done <- err
	}()
	receive(t, entered)
	if reads := body.reads.Load(); reads != 1 {
		t.Fatalf("read ahead while sink blocked: %d reads", reads)
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	assertClosed(t, body)
}

func TestRelayUnaryAndNonSuccessSSEAreValidatedBeforeHeaders(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       int
		typeHeader string
	}{
		{"json", 200, "application/json"},
		{"error SSE", 429, "text/event-stream"},
		{"redirect", 307, "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{reader: strings.NewReader("synthetic-response")}
			var events []string
			result, err := Relay(context.Background(), responseWith(body, test.code, test.typeHeader), testSink{
				headers: func(_ context.Context, code int, _ map[string]string) error {
					if body.reads.Load() < 2 || code != test.code {
						t.Fatal("headers preceded complete bounded body")
					}
					events = append(events, "headers")
					return nil
				},
				chunk: func(_ context.Context, data []byte) error { events = append(events, string(data)); return nil },
			}, nil)
			if err != nil || result.Streaming || !reflect.DeepEqual(events, []string{"headers", "synthetic-response"}) {
				t.Fatalf("result=%+v err=%v events=%v", result, err, events)
			}
			assertClosed(t, body)
		})
	}
}

type failedReader struct{ withData bool }

func (r failedReader) Read(p []byte) (int, error) {
	if r.withData {
		return copy(p, "partial"), errors.New("sensitive-upstream-detail")
	}
	return 0, errors.New("sensitive-upstream-detail")
}

func TestRelayFailuresCloseBodyAndDoNotLeakErrors(t *testing.T) {
	for _, test := range []struct {
		name                    string
		reader                  io.Reader
		streaming               bool
		headersFail, chunkFail  bool
		want                    error
		wantHeaders, wantChunks int
	}{
		{"stream read", failedReader{}, true, false, false, ErrRead, 1, 0},
		{"stream partial read", failedReader{true}, true, false, false, ErrRead, 1, 1},
		{"unary read", failedReader{true}, false, false, false, ErrRead, 0, 0},
		{"headers send", strings.NewReader("body"), true, true, false, ErrSend, 1, 0},
		{"chunk send", strings.NewReader("body"), true, false, true, ErrSend, 1, 1},
		{"unary chunk send", strings.NewReader("body"), false, false, true, ErrSend, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{reader: test.reader}
			contentType := "application/json"
			if test.streaming {
				contentType = "text/event-stream"
			}
			var headers, chunks, observations int
			_, err := Relay(context.Background(), responseWith(body, 200, contentType), testSink{
				headers: func(context.Context, int, map[string]string) error {
					headers++
					if test.headersFail {
						return errors.New("sensitive-sink-detail")
					}
					return nil
				},
				chunk: func(context.Context, []byte) error {
					chunks++
					if test.chunkFail {
						return errors.New("sensitive-sink-detail")
					}
					return nil
				},
			}, func([]byte) { observations++ })
			if err != test.want || headers != test.wantHeaders || chunks != test.wantChunks {
				t.Fatalf("err=%v headers=%d chunks=%d", err, headers, chunks)
			}
			if test.chunkFail && observations != 0 {
				t.Fatal("observer received undelivered chunk")
			}
			assertClosed(t, body)
		})
	}
}

type blockingBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, errors.New("read interrupted")
}

func (b *blockingBody) Close() error { close(b.closed); return nil }

func TestCancellationClosesBlockedReads(t *testing.T) {
	for _, unary := range []bool{false, true} {
		t.Run(map[bool]string{true: "unary", false: "relay"}[unary], func(t *testing.T) {
			blocker := &blockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
			body := &trackedBody{reader: blocker, closer: blocker}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				response := responseWith(body, 200, "text/event-stream")
				var err error
				if unary {
					_, err = ReadUnary(ctx, response)
				} else {
					_, err = Relay(ctx, response, testSink{}, nil)
				}
				done <- err
			}()
			receive(t, blocker.entered)
			cancel()
			if err := receive(t, done); err != context.Canceled {
				t.Fatalf("error=%v", err)
			}
			assertClosed(t, body)
		})
	}
}

func TestCancellationReleasesBlockedSink(t *testing.T) {
	body := &trackedBody{reader: strings.NewReader("data: {}\n\n")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Relay(ctx, responseWith(body, 200, "text/event-stream"), testSink{
			chunk: func(ctx context.Context, _ []byte) error { close(entered); <-ctx.Done(); return ctx.Err() },
		}, nil)
		done <- err
	}()
	receive(t, entered)
	cancel()
	if err := receive(t, done); err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	assertClosed(t, body)
}

func TestDeadlineClosesPendingHTTPBodyRead(t *testing.T) {
	blocker := &blockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	body := &trackedBody{reader: blocker, closer: blocker}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Relay(ctx, responseWith(body, 200, "text/event-stream"), testSink{}, nil)
		done <- err
	}()
	receive(t, blocker.entered)
	if err := receive(t, done); err != context.DeadlineExceeded {
		t.Fatalf("deadline error = %v", err)
	}
	assertClosed(t, body)
}

func TestRelayRejectsEncodingBeforeAnyOutput(t *testing.T) {
	for _, encoding := range []string{"gzip", "br", "identity, gzip", "gzip, identity"} {
		t.Run(encoding, func(t *testing.T) {
			body := &trackedBody{reader: strings.NewReader("sensitive-encoded-data")}
			response := responseWith(body, 200, "text/event-stream")
			response.Header.Set("Content-Encoding", encoding)
			called := false
			_, err := Relay(context.Background(), response, testSink{
				headers: func(context.Context, int, map[string]string) error { called = true; return nil },
				chunk:   func(context.Context, []byte) error { called = true; return nil },
			}, func([]byte) { called = true })
			if err != ErrEncoding || called || body.reads.Load() != 0 {
				t.Fatalf("err=%v output=%t", err, called)
			}
			assertClosed(t, body)
		})
	}
}
