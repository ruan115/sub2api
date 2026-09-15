// Package upstreamusage observes bounded Anthropic response usage without
// changing response bytes or making billing decisions. It never emits text or
// credentials from a response.
package upstreamusage

import (
	"encoding/json"
	"strings"
)

var basicFields = [...]string{
	"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
}

type counters struct {
	values  [7]int64
	present [7]bool
}

func (c *counters) observe(raw any) {
	u, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for i, field := range basicFields {
		c.take(i, u[field])
	}
	if detail, ok := u["cache_creation"].(map[string]any); ok {
		c.take(4, detail["ephemeral_5m_input_tokens"])
		c.take(5, detail["ephemeral_1h_input_tokens"])
	}
	if detail, ok := u["output_tokens_details"].(map[string]any); ok {
		c.take(6, detail["thinking_tokens"])
	}
}

func (c *counters) take(index int, raw any) {
	n, ok := raw.(json.Number)
	if !ok || strings.HasPrefix(string(n), "-") {
		return
	}
	value, err := n.Int64()
	if err != nil || value < 0 {
		return
	}
	// Stream usage is cumulative, not an increment. Presence is independent
	// from value: an explicit zero survives, but missing fields remain missing.
	if !c.present[index] || value > c.values[index] {
		c.values[index], c.present[index] = value, true
	}
}

func (c *counters) json() []byte {
	out := make(map[string]any)
	for i, field := range basicFields {
		if c.present[i] {
			out[field] = c.values[i]
		}
	}
	cache := make(map[string]int64)
	if c.present[4] {
		cache["ephemeral_5m_input_tokens"] = c.values[4]
	}
	if c.present[5] {
		cache["ephemeral_1h_input_tokens"] = c.values[5]
	}
	if len(cache) != 0 {
		out["cache_creation"] = cache
	}
	if c.present[6] {
		out["output_tokens_details"] = map[string]int64{"thinking_tokens": c.values[6]}
	}
	if len(out) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(out) // Only fixed keys and int64 values are present.
	return encoded
}

// FromJSON returns only recognized, nonnegative integer usage from a complete
// JSON message response. Invalid/oversize JSON, ambiguous duplicate keys, error
// responses and absent usage return nil, never fabricated zero counts. Invalid
// individual counter values are omitted without discarding other valid fields.
// The response may omit type for compatibility with existing synthetic fixtures;
// if present, type must be "message". A usage object is not itself a response.
func FromJSON(body []byte) []byte {
	doc, ok := decodeObject(body)
	if !ok {
		return nil
	}
	if _, exists := doc["error"]; exists {
		return nil
	}
	if kind, exists := doc["type"]; exists && kind != "message" {
		return nil
	}
	var found counters
	found.observe(doc["usage"])
	return found.json()
}
