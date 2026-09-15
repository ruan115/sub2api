package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUnaryLimitAndNoPartialOutput(t *testing.T) {
	for _, size := range []int{0, MaxUnaryBytes, MaxUnaryBytes + 1} {
		for _, relay := range []bool{false, true} {
			body := &trackedBody{reader: io.LimitReader(repeatedReader{}, int64(size))}
			response := responseWith(body, http.StatusTooManyRequests, "text/event-stream")
			var data []byte
			var err error
			headers := 0
			if relay {
				_, err = Relay(context.Background(), response, testSink{
					headers: func(context.Context, int, map[string]string) error { headers++; return nil },
					chunk:   func(_ context.Context, chunk []byte) error { data = append(data, chunk...); return nil },
				}, nil)
			} else {
				data, err = ReadUnary(context.Background(), response)
			}
			if size > MaxUnaryBytes {
				if err != ErrTooLarge || len(data) != 0 || headers != 0 {
					t.Fatalf("oversize relay=%t err=%v bytes=%d headers=%d", relay, err, len(data), headers)
				}
			} else if err != nil || len(data) != size {
				t.Fatalf("size=%d relay=%t bytes=%d err=%v", size, relay, len(data), err)
			}
			assertClosed(t, body)
		}
	}
}

func TestUnaryDoesNotTrustShortContentLength(t *testing.T) {
	body := &trackedBody{reader: io.LimitReader(repeatedReader{}, MaxUnaryBytes+1)}
	response := responseWith(body, 200, "application/json")
	response.ContentLength = 1
	data, err := ReadUnary(context.Background(), response)
	if err != ErrTooLarge || data != nil {
		t.Fatalf("err=%v bytes=%d", err, len(data))
	}
	assertClosed(t, body)
}

func TestUnaryRejectsOversizedContentLengthBeforeRead(t *testing.T) {
	body := &trackedBody{reader: strings.NewReader("body")}
	response := responseWith(body, 200, "application/json")
	response.ContentLength = MaxUnaryBytes + 1
	_, err := ReadUnary(context.Background(), response)
	if err != ErrTooLarge || body.reads.Load() != 0 {
		t.Fatalf("err=%v reads=%d", err, body.reads.Load())
	}
	assertClosed(t, body)
}

func TestUnaryPreservesBytesAndRejectsReadAndEncodingErrors(t *testing.T) {
	input := []byte("{\"usage\":{\"input_tokens\":1}}\n")
	body := &trackedBody{reader: bytes.NewReader(input)}
	data, err := ReadUnary(context.Background(), responseWith(body, 200, "application/json"))
	if err != nil || !bytes.Equal(data, input) {
		t.Fatalf("err=%v", err)
	}
	assertClosed(t, body)

	body = &trackedBody{reader: failedReader{true}}
	data, err = ReadUnary(context.Background(), responseWith(body, 200, "application/json"))
	if err != ErrRead || data != nil {
		t.Fatalf("read err=%v", err)
	}
	assertClosed(t, body)

	body = &trackedBody{reader: strings.NewReader("encoded")}
	response := responseWith(body, 200, "application/json")
	response.Header.Add("Content-Encoding", "identity")
	response.Header.Add("Content-Encoding", "gzip")
	data, err = ReadUnary(context.Background(), response)
	if err != ErrEncoding || data != nil || body.reads.Load() != 0 {
		t.Fatalf("encoding err=%v", err)
	}
	assertClosed(t, body)
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

func TestBrokenReaderCannotSpinWithoutProgress(t *testing.T) {
	body := &trackedBody{reader: emptyReader{}}
	_, err := Relay(context.Background(), responseWith(body, 200, "text/event-stream"), testSink{}, nil)
	if err != ErrRead || body.reads.Load() != 100 {
		t.Fatalf("err=%v reads=%d", err, body.reads.Load())
	}
	assertClosed(t, body)
}

func TestInvalidInputsAndPreCancelledContextCloseAvailableBody(t *testing.T) {
	if _, err := Relay(context.Background(), nil, testSink{}, nil); err != ErrResponse {
		t.Fatalf("nil response: %v", err)
	}
	if _, err := ReadUnary(context.Background(), &http.Response{}); err != ErrResponse {
		t.Fatalf("nil body: %v", err)
	}
	for _, kind := range []string{"nil context", "nil sink", "cancelled", "deadline"} {
		body := &trackedBody{reader: strings.NewReader("unread")}
		response := responseWith(body, 200, "text/event-stream")
		ctx := context.Background()
		var sink Sink = testSink{}
		want := ErrResponse
		switch kind {
		case "nil context":
			ctx = nil
		case "nil sink":
			sink = nil
		case "cancelled":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
			want = context.Canceled
		case "deadline":
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 0)
			defer cancel()
			want = context.DeadlineExceeded
		}
		_, err := Relay(ctx, response, sink, nil)
		if err != want || body.reads.Load() != 0 {
			t.Fatalf("%s err=%v", kind, err)
		}
		assertClosed(t, body)
	}
}
