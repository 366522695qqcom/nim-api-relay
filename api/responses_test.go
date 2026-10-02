package api

import (
	"encoding/json"
	"testing"
)

func TestResponsesToChat_StringInput(t *testing.T) {
	in := []byte(`{
		"model": "moonshotai/kimi-k3",
		"instructions": "You are helpful.",
		"input": "Hello there",
		"max_output_tokens": 100,
		"stream": true
	}`)
	out, err := responsesToChat(in)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat["model"] != "moonshotai/kimi-k3" {
		t.Errorf("model = %v", chat["model"])
	}
	if chat["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v", chat["max_tokens"])
	}
	if chat["stream"] != true {
		t.Errorf("stream = %v", chat["stream"])
	}
	msgs, _ := chat["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "You are helpful." {
		t.Errorf("system msg = %v", first)
	}
	second := msgs[1].(map[string]interface{})
	if second["role"] != "user" || second["content"] != "Hello there" {
		t.Errorf("user msg = %v", second)
	}
}

func TestResponsesToChat_ArrayInput(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"input": [
			{"role": "user", "content": "a"},
			{"type": "message", "role": "assistant", "content": "b"},
			{"type": "function_call_output", "call_id": "x", "output": "y"}
		]
	}`)
	out, err := responsesToChat(in)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (function_call_output dropped), got %d: %s", len(msgs), out)
	}
}

func TestResponsesToChat_ToolsPassThrough(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"input": "hi",
		"tools": [{"type": "function", "function": {"name": "f", "parameters": {}}}],
		"tool_choice": "auto"
	}`)
	out, err := responsesToChat(in)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := chat["tools"]; !ok {
		t.Errorf("tools not passed through")
	}
	if chat["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", chat["tool_choice"])
	}
}

func TestChatCompletionToResponse(t *testing.T) {
	in := []byte(`{
		"id": "chatcmpl-123",
		"created": 1700000000,
		"model": "m",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "hi there"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
	}`)
	out, err := chatCompletionToResponse(in)
	if err != nil {
		t.Fatalf("chatCompletionToResponse: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["object"] != "response" {
		t.Errorf("object = %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status = %v", resp["status"])
	}
	if resp["created_at"] != float64(1700000000) {
		t.Errorf("created_at = %v", resp["created_at"])
	}
	output, _ := resp["output"].([]interface{})
	if len(output) == 0 {
		t.Fatalf("no output items")
	}
	msg := output[0].(map[string]interface{})
	content, _ := msg["content"].([]interface{})
	part := content[0].(map[string]interface{})
	if part["text"] != "hi there" {
		t.Errorf("text = %v", part["text"])
	}
	usage, _ := resp["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(5) {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
}

func TestChatCompletionToResponse_FinishReasonLength(t *testing.T) {
	in := []byte(`{
		"id": "chatcmpl-9",
		"created": 0,
		"model": "m",
		"choices": [{"message": {"role": "assistant", "content": ""}, "finish_reason": "length"}],
		"usage": {}
	}`)
	out, err := chatCompletionToResponse(in)
	if err != nil {
		t.Fatalf("chatCompletionToResponse: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "incomplete" {
		t.Errorf("status = %v", resp["status"])
	}
}

func TestIsResponsesPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/responses", true},
		{"/responses", true},
		{"/v1/responses/", true},
		{"/v1/chat/completions", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := isResponsesPath(c.path); got != c.want {
			t.Errorf("isResponsesPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestTrimSlash(t *testing.T) {
	if got := trimSlash(""); got != "" {
		t.Errorf("trimSlash empty = %q", got)
	}
	if got := trimSlash("/v1"); got != "v1" {
		t.Errorf("trimSlash /v1 = %q", got)
	}
	if got := trimSlash("v1/"); got != "v1" {
		t.Errorf("trimSlash v1/ = %q", got)
	}
}

func TestResponsesInputToMessagesNonJSON(t *testing.T) {
	_, err := responsesInputToMessages([]byte(`[{"role":`))
	if err == nil {
		t.Errorf("expected error for malformed input")
	}
}

// Ensure translated outputs contain valid JSON throughout.
func TestResponsesRoundTripValidJSON(t *testing.T) {
	in := `{
		"model": "m",
		"input": [{"role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`
	out, err := responsesToChat([]byte(in))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if !json.Valid(out) {
		t.Errorf("request output not valid JSON: %s", out)
	}
	conv, err := chatCompletionToResponse([]byte(`{"id":"x","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	if err != nil {
		t.Fatalf("chatCompletionToResponse: %v", err)
	}
	if !json.Valid(conv) {
		t.Errorf("response output not valid JSON: %s", conv)
	}
}