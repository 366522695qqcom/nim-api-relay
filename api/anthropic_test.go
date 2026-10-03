package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsAnthropicMessagesPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/messages", true},
		{"/v1/messages/", true},
		{"/MESSAGES", true},
		{"/v1/chat/completions", false},
		{"/v1/responses", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := isAnthropicMessagesPath(c.path); got != c.want {
			t.Errorf("isAnthropicMessagesPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestAnthropicToChat_SystemString(t *testing.T) {
	in := []byte(`{
		"model": "meta/llama-3.3-70b",
		"system": "You are a helpful assistant.",
		"messages": [{"role": "user", "content": "Hello"}],
		"max_tokens": 128
	}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat["model"] != "meta/llama-3.3-70b" {
		t.Errorf("model = %v", chat["model"])
	}
	if chat["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v", chat["max_tokens"])
	}
	msgs, _ := chat["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "You are a helpful assistant." {
		t.Errorf("system msg = %v", first)
	}
}

func TestAnthropicToChat_SystemArray(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"system": [
			{"type": "text", "text": "Part one. "},
			{"type": "text", "text": "Part two."}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]interface{})
	first := msgs[0].(map[string]interface{})
	if got := first["content"]; got != "Part one. Part two." {
		t.Errorf("system concat = %q", got)
	}
}

func TestAnthropicToChat_MultipleMessages(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"messages": [
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "It is 4."},
			{"role": "user", "content": [{"type": "text", "text": "Thanks!"}]}
		]
	}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	third := msgs[2].(map[string]interface{})
	if third["role"] != "user" || third["content"] != "Thanks!" {
		t.Errorf("block content msg = %v", third)
	}
}

func TestAnthropicToChat_StreamAndStop(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true,
		"stop_sequences": ["END"],
		"temperature": 0.7,
		"top_p": 0.9
	}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat["stream"] != true {
		t.Errorf("stream = %v", chat["stream"])
	}
	if chat["temperature"] != float64(0.7) {
		t.Errorf("temperature = %v", chat["temperature"])
	}
	if chat["top_p"] != float64(0.9) {
		t.Errorf("top_p = %v", chat["top_p"])
	}
	stop, _ := chat["stop"].([]interface{})
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", chat["stop"])
	}
}

func TestAnthropicToChat_MaxTokensRequired(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := chat["max_tokens"]; ok {
		t.Errorf("max_tokens should be absent when missing")
	}
}

func TestChatCompletionToAnthropicContentUsage(t *testing.T) {
	in := []byte(`{
		"id": "chatcmpl-abc",
		"model": "m",
		"choices": [{"message": {"role": "assistant", "content": "hello world"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
	}`)
	out, err := chatCompletionToAnthropic(in)
	if err != nil {
		t.Fatalf("chatCompletionToAnthropic: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("msg type/role = %v/%v", msg["type"], msg["role"])
	}
	if !strings.HasPrefix(msg["id"].(string), "msg_abc") {
		t.Errorf("id = %v", msg["id"])
	}
	content, _ := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content len = %d", len(content))
	}
	part := content[0].(map[string]interface{})
	if part["type"] != "text" || part["text"] != "hello world" {
		t.Errorf("content part = %v", part)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	usage, _ := msg["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) {
		t.Errorf("usage = %v", usage)
	}
}

func TestChatCompletionToAnthropicFinishReason(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"max_tokens", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"", "end_turn"},
	}
	for _, c := range cases {
		if got := anthropicStopReason(c.finish); got != c.want {
			t.Errorf("anthropicStopReason(%q) = %q, want %q", c.finish, got, c.want)
		}
	}

	in := []byte(`{
		"id": "chatcmpl-l",
		"model": "m",
		"choices": [{"message": {"role": "assistant", "content": "x"}, "finish_reason": "length"}],
		"usage": {}
	}`)
	out, err := chatCompletionToAnthropic(in)
	if err != nil {
		t.Fatalf("chatCompletionToAnthropic: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
}

func TestChatCompletionToAnthropicToolCalls(t *testing.T) {
	in := []byte(`{
		"id": "chatcmpl-t",
		"model": "m",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\": \"beijing\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {}
	}`)
	out, err := chatCompletionToAnthropic(in)
	if err != nil {
		t.Fatalf("chatCompletionToAnthropic: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	content, _ := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content len = %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Errorf("tool block = %v", block)
	}
	input, _ := block["input"].(map[string]interface{})
	if input["city"] != "beijing" {
		t.Errorf("tool input = %v", block["input"])
	}
}