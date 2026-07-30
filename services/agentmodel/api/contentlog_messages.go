package api

import (
	"encoding/json"
	"strings"
)

// anthropicMessageAssembler reconstructs the final, non-streaming Anthropic
// Messages JSON object from the SSE event sequence, so the content log records
// one complete assistant message instead of a stream of deltas. It is fed the
// (event, data) pairs the streaming proxy already parses for usage.
//
// Best-effort by design: a malformed or unexpected event is skipped rather than
// failing, since this runs alongside — and must never disturb — the
// byte-for-byte copy forwarded to the client. finalize returns nil when nothing
// usable was seen (e.g. message_start never arrived), and the caller logs no
// content row in that case.
type anthropicMessageAssembler struct {
	message map[string]any // the `message` object from message_start
	blocks  map[int]*anthropicBlockAcc
	order   []int
}

type anthropicBlockAcc struct {
	start     map[string]any // the content_block object from content_block_start
	text      strings.Builder
	partial   strings.Builder // input_json_delta partial_json fragments (tool_use)
	thinking  strings.Builder
	signature string
}

func newAnthropicMessageAssembler() *anthropicMessageAssembler {
	return &anthropicMessageAssembler{blocks: map[int]*anthropicBlockAcc{}}
}

// feed merges one SSE event (named by `event:`) carrying `data`.
func (a *anthropicMessageAssembler) feed(event, data string) {
	if data == "" || data == "[DONE]" {
		return
	}
	switch event {
	case "message_start":
		var ev struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return
		}
		var msg map[string]any
		if json.Unmarshal(ev.Message, &msg) == nil {
			a.message = msg
		}
	case "content_block_start":
		var ev struct {
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return
		}
		if _, ok := a.blocks[ev.Index]; !ok {
			a.order = append(a.order, ev.Index)
		}
		a.blocks[ev.Index] = &anthropicBlockAcc{start: ev.ContentBlock}
	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return
		}
		blk := a.blocks[ev.Index]
		if blk == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			blk.text.WriteString(ev.Delta.Text)
		case "input_json_delta":
			blk.partial.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			blk.thinking.WriteString(ev.Delta.Thinking)
		case "signature_delta":
			blk.signature = ev.Delta.Signature
		}
	case "message_delta":
		var ev struct {
			Delta map[string]any `json:"delta"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil || a.message == nil {
			return
		}
		// message_delta carries terminal fields (stop_reason, stop_sequence) and
		// the final output-token count; fold them onto the assembled message.
		for k, v := range ev.Delta {
			a.message[k] = v
		}
		if len(ev.Usage) > 0 {
			usage, _ := a.message["usage"].(map[string]any)
			if usage == nil {
				usage = map[string]any{}
			}
			for k, v := range ev.Usage {
				usage[k] = v
			}
			a.message["usage"] = usage
		}
	}
}

// finalize materializes each content block onto the message and returns the
// complete Messages JSON, or nil when no message was assembled.
func (a *anthropicMessageAssembler) finalize() json.RawMessage {
	if a.message == nil {
		return nil
	}
	content := make([]any, 0, len(a.order))
	for _, idx := range a.order {
		blk := a.blocks[idx]
		if blk == nil || blk.start == nil {
			continue
		}
		switch blk.start["type"] {
		case "text":
			blk.start["text"] = blk.text.String()
		case "tool_use":
			// Reassemble the streamed argument JSON; fall back to the raw
			// fragment string if it doesn't parse as a complete object.
			raw := blk.partial.String()
			if raw == "" {
				blk.start["input"] = map[string]any{}
			} else {
				var input any
				if json.Unmarshal([]byte(raw), &input) == nil {
					blk.start["input"] = input
				} else {
					blk.start["input"] = raw
				}
			}
		case "thinking":
			blk.start["thinking"] = blk.thinking.String()
			if blk.signature != "" {
				blk.start["signature"] = blk.signature
			}
		}
		content = append(content, blk.start)
	}
	a.message["content"] = content

	out, err := json.Marshal(a.message)
	if err != nil {
		return nil
	}
	return out
}
