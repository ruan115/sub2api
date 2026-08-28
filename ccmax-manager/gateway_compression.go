package main

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Go only decompresses responses automatically when Transport adds the
// Accept-Encoding header itself. Clients such as httpx send that header
// explicitly, so mirror Sub2API and decode the upstream response here before
// compatibility rewriting and usage parsing.
type decompressingRoundTripper struct {
	base http.RoundTripper
}

func (t decompressingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil {
		decompressGatewayResponse(response)
	}
	return response, err
}

func (t decompressingRoundTripper) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func decompressGatewayResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	if encoding == "" {
		return
	}

	original := response.Body
	var reader io.Reader
	switch encoding {
	case "gzip":
		decoded, err := gzip.NewReader(original)
		if err != nil {
			return
		}
		reader = decoded
	case "br":
		reader = brotli.NewReader(original)
	case "deflate":
		reader = flate.NewReader(original)
	case "zstd":
		decoded, err := zstd.NewReader(original)
		if err != nil {
			return
		}
		reader = decoded.IOReadCloser()
	default:
		return
	}

	response.Body = &decompressedGatewayBody{reader: reader, original: original}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.Uncompressed = true
}

type decompressedGatewayBody struct {
	reader   io.Reader
	original io.Closer
}

func (b *decompressedGatewayBody) Read(buffer []byte) (int, error) {
	return b.reader.Read(buffer)
}

func (b *decompressedGatewayBody) Close() error {
	if closer, ok := b.reader.(io.Closer); ok {
		_ = closer.Close()
	}
	return b.original.Close()
}
