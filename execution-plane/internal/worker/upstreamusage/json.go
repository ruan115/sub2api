package upstreamusage

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

const (
	maxJSONBytes = 2 << 20
	maxJSONDepth = 64
	maxJSONNodes = 1 << 18
)

func decodeObject(body []byte) (map[string]any, bool) {
	if len(body) == 0 || len(body) > maxJSONBytes || !utf8.Valid(body) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	remaining := maxJSONNodes
	value, ok := decodeValue(d, 0, &remaining)
	if !ok {
		return nil, false
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, false
	}
	out, ok := value.(map[string]any)
	return out, ok
}

// Token decoding preserves integer precision and rejects duplicate decoded keys
// (including escaped aliases). Bounds also apply to ignored, non-usage content.
func decodeValue(d *json.Decoder, depth int, remaining *int) (any, bool) {
	if depth > maxJSONDepth || *remaining == 0 {
		return nil, false
	}
	*remaining--
	token, err := d.Token()
	if err != nil {
		return nil, false
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return token, true
	}
	switch delim {
	case '{':
		out := make(map[string]any)
		for d.More() {
			keyToken, err := d.Token()
			key, isString := keyToken.(string)
			if err != nil || !isString {
				return nil, false
			}
			if _, exists := out[key]; exists {
				return nil, false
			}
			value, ok := decodeValue(d, depth+1, remaining)
			if !ok {
				return nil, false
			}
			out[key] = value
		}
		end, err := d.Token()
		return out, err == nil && end == json.Delim('}')
	case '[':
		var out []any
		for d.More() {
			value, ok := decodeValue(d, depth+1, remaining)
			if !ok {
				return nil, false
			}
			out = append(out, value)
		}
		end, err := d.Token()
		return out, err == nil && end == json.Delim(']')
	default:
		return nil, false
	}
}
