package app

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"go.jetify.com/sse"
)

// collectSSEMessage 解析捕获的原始 SSE 字节流，将 Anthropic 流式事件重组为完整 Message JSON。
// 此函数在 io.Copy 完成后调用，不影响实时流转发路径。
// 解析失败、空流或非 Anthropic 格式均返回 "{}"。
func collectSSEMessage(rawSSE []byte) string {
	if len(rawSSE) == 0 {
		return "{}"
	}

	decoder := sse.NewDecoder(bytes.NewReader(rawSSE))
	collector := &messageCollector{
		blockBuilders: make(map[int]*blockBuilder),
	}

	var event sse.Event
	for {
		err := decoder.Decode(&event)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			break
		}
		collector.handleEvent(event)
	}

	return collector.toJSON()
}

// messageCollector 是 Anthropic SSE 事件的有状态累积器。
type messageCollector struct {
	id           string
	role         string
	model        string
	stopReason   any
	stopSequence any
	usage        map[string]any
	content      []map[string]any
	complete     bool

	blockBuilders map[int]*blockBuilder
}

// blockBuilder 是单个 content block 的增量构建器。
type blockBuilder struct {
	blockType string
	text      strings.Builder
	thinking  strings.Builder
	inputJSON strings.Builder
	signature string
	id        string
	name      string
}

func (c *messageCollector) handleEvent(event sse.Event) {
	data, ok := eventDataAsMap(event.Data)
	if !ok {
		return
	}

	eventType := event.Event
	if eventType == "" {
		if t, _ := data["type"].(string); t != "" {
			eventType = t
		}
	}

	switch eventType {
	case "message_start":
		c.handleMessageStart(data)
	case "content_block_start":
		c.handleContentBlockStart(data)
	case "content_block_delta":
		c.handleContentBlockDelta(data)
	case "content_block_stop":
		c.handleContentBlockStop(data)
	case "message_delta":
		c.handleMessageDelta(data)
	case "message_stop":
		c.complete = true
	}
}

func (c *messageCollector) handleMessageStart(data map[string]any) {
	msg, _ := data["message"].(map[string]any)
	if msg == nil {
		return
	}
	c.id, _ = msg["id"].(string)
	c.role, _ = msg["role"].(string)
	c.model, _ = msg["model"].(string)
	if u, ok := msg["usage"].(map[string]any); ok {
		c.usage = make(map[string]any)
		for k, v := range u {
			c.usage[k] = v
		}
	}
}

func (c *messageCollector) handleContentBlockStart(data map[string]any) {
	index := intFromAny(data["index"])
	block, _ := data["content_block"].(map[string]any)
	if block == nil {
		return
	}
	bb := &blockBuilder{}
	bb.blockType, _ = block["type"].(string)
	if bb.blockType == "tool_use" || bb.blockType == "server_tool_use" {
		bb.id, _ = block["id"].(string)
		bb.name, _ = block["name"].(string)
	}
	c.blockBuilders[index] = bb
}

func (c *messageCollector) handleContentBlockDelta(data map[string]any) {
	index := intFromAny(data["index"])
	bb := c.blockBuilders[index]
	if bb == nil {
		return
	}
	delta, _ := data["delta"].(map[string]any)
	if delta == nil {
		return
	}
	deltaType, _ := delta["type"].(string)
	switch deltaType {
	case "text_delta":
		if t, ok := delta["text"].(string); ok {
			bb.text.WriteString(t)
		}
	case "input_json_delta":
		if pj, ok := delta["partial_json"].(string); ok {
			bb.inputJSON.WriteString(pj)
		}
	case "thinking_delta":
		if t, ok := delta["thinking"].(string); ok {
			bb.thinking.WriteString(t)
		}
	case "signature_delta":
		if s, ok := delta["signature"].(string); ok {
			bb.signature = s
		}
	}
}

func (c *messageCollector) handleContentBlockStop(data map[string]any) {
	index := intFromAny(data["index"])
	bb := c.blockBuilders[index]
	if bb == nil {
		return
	}

	block := map[string]any{"type": bb.blockType}

	switch bb.blockType {
	case "text":
		block["text"] = bb.text.String()
	case "thinking":
		block["thinking"] = bb.thinking.String()
		if bb.signature != "" {
			block["signature"] = bb.signature
		}
	case "tool_use", "server_tool_use":
		block["id"] = bb.id
		block["name"] = bb.name
		raw := bb.inputJSON.String()
		if raw != "" {
			var parsed any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				block["input"] = parsed
			} else {
				block["input"] = raw
			}
		} else {
			block["input"] = map[string]any{}
		}
	}

	// 确保 content 切片足够长
	for len(c.content) <= index {
		c.content = append(c.content, nil)
	}
	c.content[index] = block
	delete(c.blockBuilders, index)
}

func (c *messageCollector) handleMessageDelta(data map[string]any) {
	delta, _ := data["delta"].(map[string]any)
	if delta != nil {
		if sr, ok := delta["stop_reason"]; ok {
			c.stopReason = sr
		}
		if ss, ok := delta["stop_sequence"]; ok {
			c.stopSequence = ss
		}
	}
	if u, ok := data["usage"].(map[string]any); ok {
		if c.usage == nil {
			c.usage = make(map[string]any)
		}
		for k, v := range u {
			c.usage[k] = v
		}
	}
}

// toJSON 组装完整的 Anthropic Message JSON。
// 如果没有收集到有意义的数据则返回 "{}"。
func (c *messageCollector) toJSON() string {
	if c.id == "" && len(c.content) == 0 {
		return "{}"
	}

	// 对未关闭的 blockBuilder 也尝试输出
	for index, bb := range c.blockBuilders {
		block := map[string]any{"type": bb.blockType}
		switch bb.blockType {
		case "text":
			block["text"] = bb.text.String()
		case "thinking":
			block["thinking"] = bb.thinking.String()
		case "tool_use", "server_tool_use":
			block["id"] = bb.id
			block["name"] = bb.name
			raw := bb.inputJSON.String()
			if raw != "" {
				var parsed any
				if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
					block["input"] = parsed
				} else {
					block["input"] = raw
				}
			} else {
				block["input"] = map[string]any{}
			}
		}
		for len(c.content) <= index {
			c.content = append(c.content, nil)
		}
		c.content[index] = block
	}

	// 过滤 nil 占位
	filtered := make([]map[string]any, 0, len(c.content))
	for _, b := range c.content {
		if b != nil {
			filtered = append(filtered, b)
		}
	}

	msg := map[string]any{
		"id":            c.id,
		"type":          "message",
		"role":          c.role,
		"model":         c.model,
		"content":       filtered,
		"stop_reason":   c.stopReason,
		"stop_sequence": c.stopSequence,
		"usage":         c.usage,
	}

	out, err := json.Marshal(msg)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// eventDataAsMap 将 sse.Event.Data 转换为 map[string]any。
// go.jetify.com/sse 的 Decoder 会自动对 JSON data 做反序列化，
// 所以通常 Data 已经是 map[string]any。对 sse.Raw 等情况做兜底处理。
func eventDataAsMap(data any) (map[string]any, bool) {
	switch v := data.(type) {
	case map[string]any:
		return v, true
	case sse.Raw:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			return nil, false
		}
		return m, true
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return nil, false
		}
		return m, true
	default:
		return nil, false
	}
}

// intFromAny 从 JSON 反序列化后的值中提取整数（JSON 数字默认为 float64）。
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
