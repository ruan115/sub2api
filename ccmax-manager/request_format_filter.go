package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const requestFormatBlockedCategory = "request_format_blocked"

// validateFilteredRequestFormat rejects request shapes that Anthropic is
// known to reject. It deliberately does not normalize or remove any fields.
func validateFilteredRequestFormat(body []byte) error {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid Anthropic message request")
	}
	_, hasTemperature := payload["temperature"]
	_, hasTopP := payload["top_p"]
	if hasTemperature && hasTopP {
		return fmt.Errorf("`temperature` and `top_p` cannot both be specified for this model. Please use only one.")
	}

	system, exists := payload["system"]
	if !exists || bytes.Equal(bytes.TrimSpace(system), []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(system, &text) == nil {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(system, &blocks); err != nil {
		return fmt.Errorf("system: Input does not match the expected shape.")
	}
	for index, block := range blocks {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(block, &item); err != nil {
			return fmt.Errorf("system.%d: Input does not match the expected shape.", index)
		}
		var blockType, blockText string
		if err := json.Unmarshal(item["type"], &blockType); err != nil || blockType != "text" {
			return fmt.Errorf("system.%d: Input does not match the expected shape.", index)
		}
		if err := json.Unmarshal(item["text"], &blockText); err != nil {
			return fmt.Errorf("system.%d: Input does not match the expected shape.", index)
		}
	}
	return nil
}
