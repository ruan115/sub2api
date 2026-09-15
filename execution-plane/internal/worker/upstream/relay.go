package upstream

import (
	"context"
	"mime"
	"net/http"
)

// Sink receives the HTTP response in order. Calls are synchronous: the next
// upstream read waits for Chunk to return. A sink must honor context cancellation
// and must not modify the borrowed chunk. Retaining data requires a copy before
// the call returns.
type Sink interface {
	Headers(context.Context, int, map[string]string) error
	Chunk(context.Context, []byte) error
}

type Result struct {
	RequestID string
	Streaming bool
}

// IsStreaming is the shared response classification for transport and usage
// observation. Relay rejects malformed nonempty media types before delivery.
func IsStreaming(response *http.Response) bool {
	if response == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return response.StatusCode >= 200 && response.StatusCode < 300 && err == nil && mediaType == "text/event-stream"
}

// Relay consumes an already received response and always closes its body.
// Only successful text/event-stream responses are forwarded incrementally.
// Other responses are completely validated against MaxUnaryBytes before any
// headers or bytes reach the sink. A successful return means HTTP EOF only;
// the caller must independently validate SSE terminal events and emit its RPC
// terminal response. Cumulative SSE size is not capped by MaxUnaryBytes.
//
// observe is an optional synchronous byte observer, called after successful
// delivery of each chunk. Like Sink.Chunk, it may only borrow the bytes during
// the call and must not modify them. It must not block indefinitely or perform
// external operations.
func Relay(ctx context.Context, response *http.Response, sink Sink, observe func([]byte)) (Result, error) {
	release, err := ownResponse(ctx, response)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if sink == nil {
		return Result{}, ErrResponse
	}
	result := Result{
		RequestID: response.Header.Get("X-Request-Id"),
		Streaming: IsStreaming(response),
	}
	deliver := func(chunk []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sink.Chunk(ctx, chunk); err != nil {
			return sendError(ctx)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if observe != nil {
			observe(chunk)
		}
		return nil
	}
	var payload []byte
	if !result.Streaming {
		payload, err = readUnaryBody(ctx, response)
		if err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := sink.Headers(ctx, response.StatusCode, responseHeaders(response)); err != nil {
		return result, sendError(ctx)
	}
	if result.Streaming {
		return result, readBody(ctx, response.Body, deliver)
	}
	if len(payload) > 0 {
		if err := deliver(payload); err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}

func sendError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrSend
}
