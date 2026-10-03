package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// chunkSourcedBody is an io.ReadCloser that serves SSE chunks to the stream
// translator one at a time, blocking until the test releases each chunk. This
// lets the test assert that the first delta is emitted before any later chunk
// is made available.
type chunkSourcedBody struct {
	firstEntered  chan struct{}
	firstRelease  chan struct{}
	secondEntered chan struct{}
	secondRelease chan struct{}
	chunk1, chunk2 string
	calls          int
}

func (c *chunkSourcedBody) Read(p []byte) (int, error) {
	c.calls++
	switch c.calls {
	case 1:
		close(c.firstEntered)
		<-c.firstRelease
		return copy(p, c.chunk1), nil
	case 2:
		close(c.secondEntered)
		<-c.secondRelease
		return copy(p, c.chunk2), nil
	default:
		return 0, io.EOF
	}
}

func (c *chunkSourcedBody) Close() error { return nil }

func TestStreamChatCompletionToAnthropicIncremental(t *testing.T) {
	chunk1 := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n"
	chunk2 := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"World\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	body := &chunkSourcedBody{
		firstEntered:  make(chan struct{}),
		firstRelease:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		secondRelease: make(chan struct{}),
		chunk1:        chunk1,
		chunk2:        chunk2,
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       body,
	}
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		streamChatCompletionToAnthropic(rec, resp)
	}()

	// The translator emits message_start + content_block_start before reading
	// any upstream chunk. Wait until it blocks on the first read, then assert
	// the leading events are already present.
	<-body.firstEntered
	early := rec.Body.String()
	if !strings.Contains(early, "\"message_start\"") {
		t.Errorf("message_start not emitted before any upstream chunk:\n%s", early)
	}
	if !strings.Contains(early, "\"content_block_start\"") {
		t.Errorf("content_block_start not emitted before any upstream chunk:\n%s", early)
	}

	// Release chunk1 and wait until the translator requests chunk2. By then it
	// must already have processed chunk1 and emitted its content_block_delta.
	close(body.firstRelease)
	<-body.secondEntered
	afterFirst := rec.Body.String()
	if !strings.Contains(afterFirst, "\"text\":\"Hello\"") {
		t.Errorf("first content_block_delta not emitted before chunk2 was read:\n%s", afterFirst)
	}

	// Release chunk2 + [DONE] and wait for the stream to finish.
	close(body.secondRelease)
	<-done
	final := rec.Body.String()
	for _, want := range []string{
		"\"content_block_stop\"",
		"\"message_delta\"",
		"\"stop_reason\":\"end_turn\"",
		"\"message_stop\"",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("final stream missing %s:\n%s", want, final)
		}
	}
	if !strings.Contains(final, "\"text\":\"World\"") {
		t.Errorf("second content_block_delta missing from final stream:\n%s", final)
	}
}

func TestExtractErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai-style",
			body: `{"error":{"message":"The model meta/does-not-exist does not exist","type":"invalid_request_error"}}`,
			want: "The model meta/does-not-exist does not exist",
		},
		{
			name: "anthropic-style",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`,
			want: "bad model",
		},
		{
			name: "bare-message",
			body: `{"message":"simple error"}`,
			want: "simple error",
		},
		{
			name: "unparseable-fallback",
			body: `not json at all`,
			want: "not json at all",
		},
		{
			name: "empty-fallback",
			body: `  `,
			want: "upstream request failed",
		},
	}
	for _, c := range cases {
		if got := extractErrorMessage([]byte(c.body)); got != c.want {
			t.Errorf("%s: extractErrorMessage = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWriteAnthropicError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, http.StatusBadRequest, "The model meta/does-not-exist does not exist")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q", resp.Error.Type)
	}
	if resp.Error.Message != "The model meta/does-not-exist does not exist" {
		t.Errorf("error.message = %q", resp.Error.Message)
	}
}

// roundTripperFunc adapts a function to the http.RoundTripper interface so
// tests can stub the upstream chat completions client.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// withStubUpstream temporarily replaces upstreamClient with one backed by rt
// and restores it once the returned function is called.
func withStubUpstream(rt http.RoundTripper) (restore func()) {
	orig := upstreamClient
	upstreamClient = &http.Client{Transport: rt}
	return func() { upstreamClient = orig }
}

func TestHandleMessagesConversionFailureReturnsEnvelope(t *testing.T) {
	// Upstream returns 200 with a body that cannot be converted to an Anthropic
	// message (it is not valid JSON). The handler must return an Anthropic-style
	// {"type":"error",...} envelope at HTTP 502 and must never pass the raw body
	// through with a 200.
	raw := "not json"
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(raw)),
		}, nil
	})
	restore := withStubUpstream(rt)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleMessages(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Body.String() == raw {
		t.Errorf("raw upstream body was passed through verbatim:\n%s", rec.Body.String())
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if payload.Type != "error" {
		t.Errorf("type = %q, want error", payload.Type)
	}
	if payload.Error.Message == "" {
		t.Errorf("expected non-empty error message, got body=%s", rec.Body.String())
	}
}

func TestHandleMessagesStreamingUpstreamError(t *testing.T) {
	// A streaming request whose upstream fails with a 4xx must surface as an
	// Anthropic {"type":"error",...} JSON at HTTP 4xx (before the stream branch),
	// not raw or SSE garbage.
	upErr := `{"error":{"message":"upstream exploded"}}`
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upErr)),
		}, nil
	})
	restore := withStubUpstream(rt)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleMessages(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if strings.Contains(rec.Body.String(), "data:") || strings.Contains(rec.Body.String(), "event:") {
		t.Errorf("streaming error path must not emit SSE garbage:\n%s", rec.Body.String())
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if payload.Type != "error" {
		t.Errorf("type = %q, want error", payload.Type)
	}
	if payload.Error.Message != "upstream exploded" {
		t.Errorf("error.message = %q, want %q", payload.Error.Message, "upstream exploded")
	}
}

func TestReplaceWithAnthropicError(t *testing.T) {
	// Direct check of the helper the handler uses for non-2xx upstream responses.
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad model"}}`)),
	}
	replaceWithAnthropicError(rec, resp)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error envelope: %v", err)
	}
	if payload.Type != "error" {
		t.Errorf("type = %q, want error", payload.Type)
	}
	if payload.Error.Message != "bad model" {
		t.Errorf("error.message = %q, want %q", payload.Error.Message, "bad model")
	}
}

// midStreamErrorBody serves one valid SSE line, then a hard (non-EOF) read error
// to simulate the upstream stream dying mid-way.
type midStreamErrorBody struct {
	chunk string
	once  bool
	err   error
}

func (m *midStreamErrorBody) Read(p []byte) (int, error) {
	if !m.once {
		m.once = true
		return copy(p, m.chunk), nil
	}
	return 0, m.err
}

func (m *midStreamErrorBody) Close() error { return nil }

func TestStreamChatCompletionToAnthropicMidStreamError(t *testing.T) {
	chunk := `data: {"model":"m","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body: &midStreamErrorBody{
			chunk: chunk,
			err:   errors.New("connection reset by peer"),
		},
	}
	rec := httptest.NewRecorder()
	streamChatCompletionToAnthropic(rec, resp)

	out := rec.Body.String()
	for _, want := range []string{
		"message_start",
		"content_block_delta",
		"\"text\":\"Hello\"",
		"content_block_stop",
		"message_delta",
		"event: error",
		"api_error",
		"message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mid-stream failure output missing %q:\n%s", want, out)
		}
	}
	// The error SSE event must be emitted before message_stop closes the stream.
	if iErr, iStop := strings.Index(out, "event: error"), strings.Index(out, "message_stop"); iErr < 0 || iStop < 0 || iErr > iStop {
		t.Errorf("expected error event before message_stop:\n%s", out)
	}
	// No raw Chat Completions chunk JSON should be forwarded as a data: payload.
	if strings.Contains(out, "finish_reason") {
		t.Errorf("raw chat completion chunk leaked into stream:\n%s", out)
	}
}

func TestHandleMessagesSuccessNonStream(t *testing.T) {
	// Regression: an upstream 200 non-stream body must be converted to an
	// Anthropic message envelope (top-level "type":"message"), not passed raw.
	chat := `{
		"id": "chatcmpl-abc",
		"model": "m",
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}`
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(chat)),
		}, nil
	})
	restore := withStubUpstream(rt)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "message" {
		t.Errorf("type = %q, want message", payload.Type)
	}
	if !strings.HasPrefix(payload.ID, "msg_") {
		t.Errorf("id = %q, want msg_ prefix", payload.ID)
	}
	if payload.Model != "m" {
		t.Errorf("model = %q, want m", payload.Model)
	}
}

func TestLogMessagesDiagnosticSmoke(t *testing.T) {
	// Sanity: the diagnostic logger must not panic and must never embed the API
	// key. (It simply formats a line via the standard logger.)
	logMessagesDiagnostic("m", true, 400, "bad upstream", "", 42)
	logMessagesDiagnostic("m", false, 502, "", "bad conversion", 0)
}