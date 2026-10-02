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

func TestNormalizeAPIPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/chat/completions", "/v1/chat/completions"},
		{"/Chat/Completions", "/v1/chat/completions"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/responses", "/responses"},
		{"/v1/responses", "/v1/responses"},
		{"/models", "/models"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := normalizeAPIPath(c.in); got != c.want {
			t.Errorf("normalizeAPIPath(%q) = %q, want %q", c.in, got, c.want)
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

func TestNormalizeMessageContent(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{
			name: "string passes through",
			in:   "hi",
			want: "hi",
		},
		{
			name: "non-string non-array passes through",
			in:   map[string]interface{}{"foo": "bar"},
			want: map[string]interface{}{"foo": "bar"},
		},
		{
			name: "nil maps to empty string",
			in:   nil,
			want: nil,
		},
		{
			name: "number passes through",
			in:   float64(42),
			want: float64(42),
		},
		{
			name: "input_text becomes text",
			in: []interface{}{
				map[string]interface{}{"type": "input_text", "text": "hi", "annotations": []interface{}{}},
			},
			want: []interface{}{
				map[string]interface{}{"type": "text", "text": "hi", "annotations": []interface{}{}},
			},
		},
		{
			name: "output_text becomes text",
			in: []interface{}{
				map[string]interface{}{"type": "output_text", "text": "bye"},
			},
			want: []interface{}{
				map[string]interface{}{"type": "text", "text": "bye"},
			},
		},
		{
			name: "input_image with image_url object",
			in: []interface{}{
				map[string]interface{}{"type": "input_image", "image_url": map[string]interface{}{"url": "http://x"}},
			},
			want: []interface{}{
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://x"}},
			},
		},
		{
			name: "input_image_url with url promoted to image_url",
			in: []interface{}{
				map[string]interface{}{"type": "input_image_url", "url": "http://y"},
			},
			want: []interface{}{
				map[string]interface{}{"type": "image_url", "url": "http://y", "image_url": "http://y"},
			},
		},
		{
			name: "already compatible blocks pass through",
			in: []interface{}{
				map[string]interface{}{"type": "text", "text": "a"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://z"}},
			},
			want: []interface{}{
				map[string]interface{}{"type": "text", "text": "a"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://z"}},
			},
		},
		{
			name: "unmappable and non-map blocks dropped",
			in: []interface{}{
				"not-a-map",
				map[string]interface{}{"type": "function_call", "id": "x"},
			},
			want: "",
		},
		{
			name: "empty array maps to empty string",
			in:   []interface{}{},
			want: "",
		},
		{
			name: "mixed: keep compatible, map, drop",
			in: []interface{}{
				map[string]interface{}{"type": "text", "text": "k"},
				map[string]interface{}{"type": "input_text", "text": "m"},
				map[string]interface{}{"type": "function_call"},
			},
			want: []interface{}{
				map[string]interface{}{"type": "text", "text": "k"},
				map[string]interface{}{"type": "text", "text": "m"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeMessageContent(c.in)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("normalizeMessageContent(%v)\n got %s\nwant %s", c.in, gotJSON, wantJSON)
			}
		})
	}
}

func TestNormalizeMessageContentNoMutation(t *testing.T) {
	in := []interface{}{
		map[string]interface{}{"type": "input_text", "text": "hi"},
	}
	copy := []interface{}{
		map[string]interface{}{"type": "input_text", "text": "hi"},
	}
	got := normalizeMessageContent(in)
	gotArr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{} result, got %T", got)
	}
	block := gotArr[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("result block type = %v, want text", block["type"])
	}
	// Original input must be unchanged.
	origJSON, _ := json.Marshal(in)
	copyJSON, _ := json.Marshal(copy)
	if string(origJSON) != string(copyJSON) {
		t.Errorf("input mutated:\n got %s\nwant %s", origJSON, copyJSON)
	}
}

func TestResponsesToChat_ContentBlockNormalizationAndDeveloperRole(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"role": "developer", "content": [{"type": "output_text", "text": "sys"}]},
			{"role": "assistant", "content": [{"type": "input_image", "image_url": {"url": "http://i"}}]},
			{"role": "user", "content": [{"type": "function_call", "id": "x"}]},
			{"role": "user", "content": "plain"}
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
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d: %s", len(msgs), out)
	}

	first := msgs[0].(map[string]interface{})
	if first["role"] != "user" {
		t.Errorf("msg0 role = %v", first["role"])
	}
	c0, _ := first["content"].([]interface{})
	b0 := c0[0].(map[string]interface{})
	if b0["type"] != "text" || b0["text"] != "hi" {
		t.Errorf("msg0 block = %v", b0)
	}

	second := msgs[1].(map[string]interface{})
	if second["role"] != "system" {
		t.Errorf("msg1 role = %v, want system (developer normalized)", second["role"])
	}
	c1, _ := second["content"].([]interface{})
	b1 := c1[0].(map[string]interface{})
	if b1["type"] != "text" || b1["text"] != "sys" {
		t.Errorf("msg1 block = %v", b1)
	}

	third := msgs[2].(map[string]interface{})
	c2, _ := third["content"].([]interface{})
	b2 := c2[0].(map[string]interface{})
	if b2["type"] != "image_url" {
		t.Errorf("msg2 block type = %v", b2["type"])
	}
	img, _ := b2["image_url"].(map[string]interface{})
	if img["url"] != "http://i" {
		t.Errorf("msg2 image_url = %v", img)
	}

	fourth := msgs[3].(map[string]interface{})
	if fourth["content"] != "" {
		t.Errorf("msg3 content = %v, want empty string", fourth["content"])
	}

	fifth := msgs[4].(map[string]interface{})
	if fifth["content"] != "plain" {
		t.Errorf("msg4 content = %v, want plain", fifth["content"])
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