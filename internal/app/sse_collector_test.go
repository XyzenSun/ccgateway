package app

import (
	"encoding/json"
	"fmt"
	"testing"
)

const testSSERaw = `event: message_start
data: {"type": "message_start", "message": {"id": "msg_89d8e9895215408cb73fea18", "type": "message", "role": "assistant", "model": "deepseek-ai/DeepSeek-V4-Flash", "content": [], "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 0, "output_tokens": 0}}}

event: content_block_start
data: {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "hello"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": " world"}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 0}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": null}, "usage": {"input_tokens": 10, "output_tokens": 29}}

event: message_stop
data: {"type": "message_stop"}

`

func TestCollectSSEMessage(t *testing.T) {
	result := collectSSEMessage([]byte(testSSERaw))
	fmt.Printf("Result: %s\n", result)

	if result == "{}" {
		t.Fatal("collectSSEMessage returned empty '{}', expected collected message")
	}

	var msg map[string]any
	if err := json.Unmarshal([]byte(result), &msg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if msg["id"] != "msg_89d8e9895215408cb73fea18" {
		t.Errorf("id mismatch: got %v", msg["id"])
	}

	content, _ := msg["content"].([]any)
	if len(content) == 0 {
		t.Fatal("content is empty")
	}

	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if text != "hello world" {
		t.Errorf("text mismatch: got %q, want %q", text, "hello world")
	}

	t.Logf("Collected message: %s", result)
}

func TestCollectSSEDebug(t *testing.T) {
	// Debug: parse events one by one
	raw := []byte(testSSERaw)
	fmt.Printf("Raw SSE length: %d bytes\n", len(raw))
	fmt.Printf("First 200 bytes: %q\n", string(raw[:200]))

	result := collectSSEMessage(raw)
	fmt.Printf("collectSSEMessage result: %s\n", result)
}
