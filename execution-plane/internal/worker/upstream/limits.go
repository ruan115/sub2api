// Package upstream consumes HTTP responses without owning request routing,
// credentials, retries, or the worker's RPC protocol.
package upstream

import "errors"

const (
	// ChunkBytes bounds one streaming read, not the cumulative response size.
	ChunkBytes = 32 << 10
	// MaxUnaryBytes bounds complete non-streaming responses, including errors.
	MaxUnaryBytes = 2 << 20
)

var (
	ErrResponse = errors.New("upstream response configuration is invalid")
	ErrRead     = errors.New("upstream response read failed")
	ErrTooLarge = errors.New("upstream unary response exceeded size limit")
	ErrEncoding = errors.New("upstream response encoding is unsupported")
	ErrSend     = errors.New("upstream response delivery failed")
)
