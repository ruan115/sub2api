package upstream

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// ownResponse always closes an available body, including on validation errors.
// Closing the HTTP body on cancellation interrupts a pending HTTP body Read.
// Injected test bodies must obey the same concurrent Read/Close contract.
func ownResponse(ctx context.Context, response *http.Response) (func(), error) {
	if response == nil || response.Body == nil {
		return nil, ErrResponse
	}
	var once sync.Once
	closeBody := func() { once.Do(func() { _ = response.Body.Close() }) }
	if ctx == nil {
		closeBody()
		return nil, ErrResponse
	}
	stop := context.AfterFunc(ctx, closeBody)
	release := func() {
		stop()
		closeBody()
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			release()
			return nil, ErrResponse
		}
	}
	for _, value := range response.Header.Values("Content-Encoding") {
		value = strings.TrimSpace(value)
		if value != "" && !strings.EqualFold(value, "identity") {
			release()
			return nil, ErrEncoding
		}
	}
	return release, nil
}

func responseHeaders(response *http.Response) map[string]string {
	headers := make(map[string]string, 2)
	for _, name := range []string{"Content-Type", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			headers[strings.ToLower(name)] = value
		}
	}
	return headers
}

func readError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrRead
}

// readBody makes only synchronous, bounded reads. The caller owns body closure.
// Unlike io.ReadAll, it also detects a broken reader returning no progress.
func readBody(ctx context.Context, body io.Reader, consume func([]byte) error) error {
	buffer := make([]byte, ChunkBytes)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := body.Read(buffer)
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if n > 0 {
			emptyReads = 0
			if consumeErr := consume(buffer[:n]); consumeErr != nil {
				return consumeErr
			}
		} else if err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return ErrRead
			}
		}
		if err == io.EOF {
			return ctx.Err()
		}
		if err != nil {
			return readError(ctx)
		}
	}
}
