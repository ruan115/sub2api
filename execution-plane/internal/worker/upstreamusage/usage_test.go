package upstreamusage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func assertUsage(t *testing.T, got []byte, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("expected absent usage, got %s", got)
		}
		return
	}
	var wanted any
	d := json.NewDecoder(strings.NewReader(want))
	d.UseNumber()
	if err := d.Decode(&wanted); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(wanted)
	if !bytes.Equal(got, encoded) {
		t.Fatalf("usage = %s; want %s", got, encoded)
	}
}

func TestFromJSONWhitelistedUsageOnly(t *testing.T) {
	body := []byte(`{"type":"message","content":[{"text":"synthetic_secret"}],"usage":{"input_tokens":20,"output_tokens":27,"cache_creation_input_tokens":7,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":5,"extra":"synthetic_secret"},"output_tokens_details":{"thinking_tokens":18,"extra":99},"iterations":[{"input_tokens":9999}],"text":"synthetic_secret"}}`)
	copyBefore := append([]byte(nil), body...)
	assertUsage(t, FromJSON(body), `{"input_tokens":20,"output_tokens":27,"cache_creation_input_tokens":7,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":5},"output_tokens_details":{"thinking_tokens":18}}`)
	if !bytes.Equal(body, copyBefore) {
		t.Fatal("observer changed response")
	}
}

func TestFromJSONMissingInvalidAndAmbiguous(t *testing.T) {
	for name, body := range map[string]string{
		"missing":              `{"content":[]}`,
		"empty":                `{"usage":{}}`,
		"nil":                  `{"usage":null}`,
		"string":               `{"usage":"synthetic_secret"}`,
		"standalone usage":     `{"input_tokens":7}`,
		"deep spoof":           `{"content":[{"usage":{"input_tokens":7}}]}`,
		"error type":           `{"type":"error","usage":{"input_tokens":7}}`,
		"error object":         `{"error":{},"usage":{"input_tokens":7}}`,
		"bad type":             `{"type":[],"usage":{"input_tokens":7}}`,
		"trailing":             `{"usage":{"input_tokens":7}}{}`,
		"duplicate usage":      `{"usage":{"input_tokens":7},"usage":{"input_tokens":8}}`,
		"escaped duplicate":    `{"usage":{"input_tokens":7,"input_\u0074okens":8}}`,
		"duplicate detail":     `{"usage":{"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_5m_input_tokens":2}}}`,
		"malformed":            `{"usage":{"input_tokens":7}`,
		"invalid UTF8":         "{\"usage\":{\"input_tokens\":7},\"text\":\"\xff\"}",
		"all invalid counters": `{"usage":{"input_tokens":-1,"output_tokens":1.1,"cache_creation_input_tokens":"3","cache_read_input_tokens":9223372036854775808}}`,
	} {
		t.Run(name, func(t *testing.T) { assertUsage(t, FromJSON([]byte(body)), "") })
	}
}

func TestFromJSONRetainsExplicitZeroAndValidIntegersOnly(t *testing.T) {
	for _, invalid := range []string{"-1", "-0", "0.0", "1.5", "1e0", "true", "null", `"42"`, "[]", "{}", "9223372036854775808", "18446744073709551616"} {
		t.Run(invalid, func(t *testing.T) {
			assertUsage(t, FromJSON([]byte(`{"usage":{"input_tokens":`+invalid+`,"output_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":`+invalid+`,"ephemeral_1h_input_tokens":2},"output_tokens_details":{"thinking_tokens":`+invalid+`}}}`)), `{"output_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":2}}`)
		})
	}
	assertUsage(t, FromJSON([]byte(`{"usage":{"input_tokens":9223372036854775807}}`)), `{"input_tokens":9223372036854775807}`)
}

func TestFromJSONBoundsAndLargeUnknownContent(t *testing.T) {
	prefix, suffix := `{"content":"`, `","usage":{"input_tokens":5}}`
	atLimit := []byte(prefix + strings.Repeat("x", maxJSONBytes-len(prefix)-len(suffix)) + suffix)
	assertUsage(t, FromJSON(atLimit), `{"input_tokens":5}`)
	assertUsage(t, FromJSON(append(atLimit, ' ')), "")
	deep := `{"usage":{"input_tokens":5},"other":` + strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1) + "}"
	assertUsage(t, FromJSON([]byte(deep)), "")
	many := `{"usage":{"input_tokens":5},"other":[` + strings.Repeat("0,", maxJSONNodes) + "0]}"
	assertUsage(t, FromJSON([]byte(many)), "")
}

func FuzzFromJSON(f *testing.F) {
	f.Add([]byte(`{"usage":{"input_tokens":0}}`))
	f.Add([]byte(`{"usage":{"input_tokens":1,"input_tokens":2}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		before := append([]byte(nil), body...)
		usage := FromJSON(body)
		if !bytes.Equal(body, before) || (usage != nil && !json.Valid(usage)) {
			t.Fatal("invalid observer result")
		}
	})
}
