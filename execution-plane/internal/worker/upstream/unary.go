package upstream

import (
	"context"
	"net/http"
)

// ReadUnary returns a complete bounded response body, or no body on failure.
// It owns and always closes response.Body; it never sends or logs its contents.
func ReadUnary(ctx context.Context, response *http.Response) ([]byte, error) {
	release, err := ownResponse(ctx, response)
	if err != nil {
		return nil, err
	}
	defer release()
	return readUnaryBody(ctx, response)
}

func readUnaryBody(ctx context.Context, response *http.Response) ([]byte, error) {
	if response.ContentLength > MaxUnaryBytes {
		return nil, ErrTooLarge
	}
	var payload []byte
	err := readBody(ctx, response.Body, func(chunk []byte) error {
		if len(chunk) > MaxUnaryBytes-len(payload) {
			return ErrTooLarge
		}
		payload = append(payload, chunk...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}
