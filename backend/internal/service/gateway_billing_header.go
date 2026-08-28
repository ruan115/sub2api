package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ccVersionInBillingRe matches the complete cc_version value, including the
// request-derived fingerprint suffix when present.
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+(?:\.[A-Za-z0-9_-]+)?`)

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent. The
// fingerprint includes the CLI version, so it must be recomputed as one atomic
// identity value instead of retaining the suffix generated for an older UA.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".
func syncBillingHeaderVersion(body []byte, userAgent string) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	replacement := "cc_version=" + version + "." + computeClaudeCodeFingerprint(body, version)
	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			newText := ccVersionInBillingRe.ReplaceAllString(text.String(), replacement)
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})

	return body
}

// syncRequestBillingHeaderVersion runs only after all outgoing headers are
// finalized. It keeps the request body and ContentLength in sync while touching
// only an existing billing attribution system block.
func syncRequestBillingHeaderVersion(req *http.Request, body []byte) []byte {
	if req == nil {
		return body
	}
	updated := syncBillingHeaderVersion(body, getHeaderRaw(req.Header, "User-Agent"))
	if bytes.Equal(updated, body) {
		return body
	}
	req.Body = io.NopCloser(bytes.NewReader(updated))
	req.ContentLength = int64(len(updated))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(updated)), nil
	}
	return updated
}
