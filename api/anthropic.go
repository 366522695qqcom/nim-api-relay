package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// isAnthropicMessagesPath reports whether the request targets the Anthropic
// Messages API path. It mirrors isResponsesPath.
func isAnthropicMessagesPath(path string) bool {
	p := strings.TrimRight(strings.ToLower(path), "/")
	return p == "/v1/messages" || p == "/messages"
}

// anthropicRequest carries the Anthropic Messages API request fields that a
// relay needs to translate into a Chat Completions request. Unknown fields are
// intentionally dropped.
type anthropicRequest struct {
	Model         string          `json:"model"`
	Messages      json.RawMessage `json:"messages"`
	System        json.RawMessage `json:"system"`
	MaxTokens     *int            `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	Tools         json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	StopSequences json.RawMessage `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
}

// anthropicSystemText flattens the Anthropic `system` value (either a plain
// string or an array of {type:"text",text} blocks) into a single text string.
func anthropicSystemText(system json.RawMessage) string {
	if len(bytes.TrimSpace(system)) == 0 || bytes.Equal(bytes.TrimSpace(system), []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(system, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(system, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicContentToChat flattens an Anthropic message `content` (a string or
// an array of blocks) into plain chat content. Text blocks are concatenated and
// image blocks are converted into the Chat Completions image_url form.
func anthropicContentToChat(content interface{}) interface{} {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var text strings.Builder
		var parts []interface{}
		for _, p := range v {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			switch t, _ := m["type"].(string); t {
			case "", "text":
				text.WriteString(stringValue(m["text"]))
			default:
				// image / image_url blocks must be restructured for OpenAI:
				// Anthropic uses {"type":"image","source":{"type":"base64",…}}
				// while Chat Completions expects {"type":"image_url","image_url":{…}}.
				if part, ok := anthropicContentPartToChat(m); ok {
					parts = append(parts, part)
				}
			}
		}
		if len(parts) > 0 {
			if text.Len() > 0 {
				parts = append([]interface{}{
					map[string]interface{}{"type": "text", "text": text.String()},
				}, parts...)
			}
			return parts
		}
		return text.String()
	default:
		return content
	}
}

// mappedModel translates a client-supplied model name into the upstream NIM
// model id. MODEL_MAP holds comma/semicolon/newline separated "from=to" pairs;
// DEFAULT_MODEL is the fallback for any claude-* name without an explicit entry.
// Unknown names pass through unchanged.
func mappedModel(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return m
	}
	for _, pair := range strings.FieldsFunc(os.Getenv("MODEL_MAP"), func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	}) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), m) {
			if to := strings.TrimSpace(v); to != "" {
				return to
			}
		}
	}
	if strings.HasPrefix(strings.ToLower(m), "claude") {
		if def := strings.TrimSpace(os.Getenv("DEFAULT_MODEL")); def != "" {
			return def
		}
	}
	return m
}

// anthropicToolResultText flattens a tool_result `content` value (a string or
// an array of text blocks) into plain text.
func anthropicToolResultText(v interface{}) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []interface{}:
		var sb strings.Builder
		for _, p := range c {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" || t == "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(stringValue(m["text"]))
			}
		}
		return sb.String()
	default:
		return stringOrJSON(c)
	}
}

// anthropicContentPartToChat converts a non-text Anthropic content block such
// as an image into the Chat Completions equivalent.
func anthropicContentPartToChat(m map[string]interface{}) (interface{}, bool) {
	switch t, _ := m["type"].(string); t {
	case "image":
		src, ok := m["source"].(map[string]interface{})
		if !ok {
			return nil, false
		}
		switch st, _ := src["type"].(string); st {
		case "base64":
			media, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			if data == "" {
				return nil, false
			}
			if media == "" {
				media = "image/png"
			}
			return map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": "data:" + media + ";base64," + data},
			}, true
		case "url":
			if u, _ := src["url"].(string); u != "" {
				return map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]interface{}{"url": u},
				}, true
			}
		}
		return nil, false
	case "image_url":
		return m, true
	}
	return nil, false
}

// anthropicAssistantMessage maps one Anthropic assistant message into a Chat
// Completions assistant message, turning tool_use blocks into tool_calls.
func anthropicAssistantMessage(content interface{}) map[string]interface{} {
	msg := map[string]interface{}{"role": "assistant"}
	if s, ok := content.(string); ok {
		msg["content"] = s
		return msg
	}
	blocks, ok := content.([]interface{})
	if !ok {
		msg["content"] = anthropicContentToChat(content)
		return msg
	}
	var text strings.Builder
	var calls []interface{}
	for _, p := range blocks {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		switch t, _ := m["type"].(string); t {
		case "tool_use":
			args, err := json.Marshal(m["input"])
			if err != nil || len(args) == 0 || string(args) == "null" {
				args = []byte("{}")
			}
			calls = append(calls, map[string]interface{}{
				"id":   stringValue(m["id"]),
				"type": "function",
				"function": map[string]interface{}{
					"name":      stringValue(m["name"]),
					"arguments": string(args),
				},
			})
		case "text", "":
			text.WriteString(stringValue(m["text"]))
		}
	}
	msg["content"] = text.String()
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return msg
}

// anthropicUserMessages maps one Anthropic user message into zero or more Chat
// Completions messages: tool_result blocks become role:"tool" messages, while
// text/image blocks become a user message. A single Anthropic message can
// therefore expand into several OpenAI messages.
func anthropicUserMessages(content interface{}) []map[string]interface{} {
	if s, ok := content.(string); ok {
		return []map[string]interface{}{{"role": "user", "content": s}}
	}
	blocks, ok := content.([]interface{})
	if !ok {
		return []map[string]interface{}{{"role": "user", "content": anthropicContentToChat(content)}}
	}

	var out []map[string]interface{}
	var text strings.Builder
	var parts []interface{}
	hasText := false
	flush := func() {
		if !hasText && len(parts) == 0 {
			return
		}
		var c interface{}
		if len(parts) == 0 {
			c = text.String()
		} else {
			if hasText {
				parts = append([]interface{}{
					map[string]interface{}{"type": "text", "text": text.String()},
				}, parts...)
			}
			c = parts
		}
		out = append(out, map[string]interface{}{"role": "user", "content": c})
		text.Reset()
		hasText = false
		parts = nil
	}

	for _, p := range blocks {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		switch t, _ := m["type"].(string); t {
		case "tool_result":
			flush()
			txt := anthropicToolResultText(m["content"])
			if isErr, _ := m["is_error"].(bool); isErr && txt != "" {
				txt = "Error: " + txt
			}
			out = append(out, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": stringValue(m["tool_use_id"]),
				"content":      txt,
			})
		case "text", "":
			text.WriteString(stringValue(m["text"]))
			hasText = true
		default:
			if part, ok := anthropicContentPartToChat(m); ok {
				parts = append(parts, part)
			}
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, map[string]interface{}{"role": "user", "content": ""})
	}
	return out
}

// anthropicMessagesToChat maps Anthropic `messages` (each with role
// user/assistant and a string or array content) to Chat Completions messages.
func anthropicMessagesToChat(in json.RawMessage) ([]map[string]interface{}, error) {
	var arr []struct {
		Role    string      `json:"role"`
		Content interface{} `json:"content"`
	}
	if err := json.Unmarshal(in, &arr); err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(arr))
	for _, m := range arr {
		switch m.Role {
		case "assistant":
			out = append(out, anthropicAssistantMessage(m.Content))
		case "user", "":
			out = append(out, anthropicUserMessages(m.Content)...)
		default:
			out = append(out, map[string]interface{}{
				"role":    m.Role,
				"content": stringValue(m.Content),
			})
		}
	}
	return out, nil
}

// anthropicToolsToChat converts Anthropic tool definitions
// ({name,description,input_schema}) into OpenAI function tools
// ({type:"function",function:{...}}). Already-OpenAI-shaped entries pass through.
func anthropicToolsToChat(raw json.RawMessage) interface{} {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var arr []map[string]interface{}
	if json.Unmarshal(raw, &arr) != nil || len(arr) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(arr))
	for _, t := range arr {
		if typ, _ := t["type"].(string); typ == "function" {
			out = append(out, t)
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]interface{}{"name": name}
		if d, _ := t["description"].(string); d != "" {
			fn["description"] = d
		}
		params := t["input_schema"]
		if _, ok := params.(map[string]interface{}); !ok {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		fn["parameters"] = params
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoiceToChat converts Anthropic tool_choice into the OpenAI form:
// auto → "auto", any → "required", none → omitted, tool → named function.
func anthropicToolChoiceToChat(raw json.RawMessage) interface{} {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil
	}
	var s string
	if json.Unmarshal(trim, &s) == nil {
		switch s {
		case "auto":
			return "auto"
		case "any", "required":
			return "required"
		case "none":
			return nil
		}
		return s
	}
	var o map[string]interface{}
	if json.Unmarshal(trim, &o) == nil {
		if _, ok := o["function"]; ok {
			return o
		}
		switch typ, _ := o["type"].(string); typ {
		case "auto":
			return "auto"
		case "any", "required":
			return "required"
		case "none":
			return nil
		case "tool":
			if name, _ := o["name"].(string); name != "" {
				return map[string]interface{}{
					"type":     "function",
					"function": map[string]interface{}{"name": name},
				}
			}
		}
	}
	return nil
}

// anthropicToChat translates an Anthropic Messages request body into a Chat
// Completions request body.
func anthropicToChat(input []byte) ([]byte, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(input, &ar); err != nil {
		return nil, err
	}

	chat := map[string]interface{}{"model": mappedModel(ar.Model)}

	messages := make([]map[string]interface{}, 0, 4)
	if sys := anthropicSystemText(ar.System); sys != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": sys})
	}
	conv, err := anthropicMessagesToChat(ar.Messages)
	if err != nil {
		return nil, err
	}
	messages = append(messages, conv...)
	chat["messages"] = messages

	if ar.MaxTokens != nil {
		chat["max_tokens"] = *ar.MaxTokens
	}
	if ar.Temperature != nil {
		chat["temperature"] = *ar.Temperature
	}
	if ar.TopP != nil {
		chat["top_p"] = *ar.TopP
	}
	if tools := anthropicToolsToChat(ar.Tools); tools != nil {
		chat["tools"] = tools
	}
	if tc := anthropicToolChoiceToChat(ar.ToolChoice); tc != nil {
		chat["tool_choice"] = tc
	}
	setRawField(chat, "stop", ar.StopSequences)
	if ar.Stream {
		chat["stream"] = true
	}

	return json.Marshal(chat)
}

// anthropicStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func anthropicStopReason(finish string) string {
	switch finish {
	case "", "stop":
		return "end_turn"
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthropicMessageID generates a fresh Anthropic message id.
func anthropicMessageID() string {
	return "msg_" + time.Now().UTC().Format("20060102150405.000000000")
}

// chatCompletionToAnthropic converts a non-streaming Chat Completions response
// into an Anthropic message object.
func chatCompletionToAnthropic(in []byte) ([]byte, error) {
	var co struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   interface{}       `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(in, &co); err != nil {
		return nil, err
	}

	var content []interface{}
	stopReason := "end_turn"
	if len(co.Choices) > 0 {
		ch := co.Choices[0]
		stopReason = anthropicStopReason(ch.FinishReason)
		if text := contentToString(ch.Message.Content); text != "" {
			content = append(content, map[string]interface{}{
				"type": "text", "text": text,
			})
		}
		for _, tc := range ch.Message.ToolCalls {
			var call map[string]interface{}
			if json.Unmarshal(tc, &call) != nil {
				continue
			}
			fn, _ := call["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			var input interface{}
			_ = json.Unmarshal([]byte(stringValue(fn["arguments"])), &input)
			if input == nil {
				input = stringValue(fn["arguments"])
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    call["id"],
				"name":  name,
				"input": input,
			})
		}
	}
	if content == nil {
		content = []interface{}{}
	}

	resp := map[string]interface{}{
		"type":          "message",
		"id":            "msg_" + strings.TrimPrefix(co.ID, "chatcmpl-"),
		"model":         co.Model,
		"role":          "assistant",
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  co.Usage.PromptTokens,
			"output_tokens": co.Usage.CompletionTokens,
		},
	}
	return json.Marshal(resp)
}

// writeAnthropicError writes a standard Anthropic-style error response.
func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	payload := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "invalid_request_error",
			"message": message,
		},
	}
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// isEventStream reports whether a Content-Type denotes an SSE stream.
func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// extractErrorMessage pulls a human-readable message out of an upstream error
// body. It handles OpenAI/NVIDIA-style {"error":{"message":...}} and
// Anthropic-style {"error":{"type":...,"message":...}} shapes plus a bare
// top-level "message", and falls back to the raw body (truncated).
func extractErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return m.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 2000 {
		s = s[:2000]
	}
	if s == "" {
		return "upstream request failed"
	}
	return s
}

// replaceWithAnthropicError reads an upstream non-2xx error body and writes a
// standard Anthropic-style error response, preserving the upstream status code.
func replaceWithAnthropicError(w http.ResponseWriter, resp *http.Response) {
	body, _ := readAllCloser(resp.Body)
	writeAnthropicError(w, resp.StatusCode, extractErrorMessage(body))
}

// handleMessages translates a /v1/messages request into a Chat Completions
// request, forwards it to the upstream, and converts the response back to the
// Anthropic Messages protocol.
func handleMessages(w http.ResponseWriter, r *http.Request) {
	// Wrap w to count response bytes for the diagnostics log. The wrapper
	// forwards Flush so the streaming path keeps flushing to the client.
	counting := &byteCountWriter{ResponseWriter: w}
	w = counting

	// Diagnostics facts captured across the handler's various return points and
	// emitted as a single structured line after the request completes.
	var diagModel, diagUpErr, diagConvertErr string
	var diagStream bool
	var diagUpstream int
	defer func() {
		logMessagesDiagnostic(diagModel, diagStream, diagUpstream, diagUpErr, diagConvertErr, counting.n)
	}()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		if isBodyTooLarge(err) {
			writeBodyTooLarge(w, r.URL.Path)
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "failed to read request body")
		return
	}

	// Record the request model (best-effort) for the diagnostics log.
	var ar anthropicRequest
	if json.Unmarshal(bodyBytes, &ar) == nil {
		diagModel = ar.Model
	}

	chatBody, err := anthropicToChat(bodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid messages request: "+err.Error())
		return
	}

	target := upstreamURL()
	req, err := buildUpstreamRequest(target, r, bytes.NewReader(chatBody))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "failed to build upstream request")
		return
	}
	req.URL.Path = trimSlash(target.Path) + "/v1/chat/completions"

	// Anthropic authenticates with x-api-key; convert it to the upstream's
	// bearer Authorization form if present.
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	isStream := false
	var p struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(bodyBytes, &p) == nil {
		isStream = p.Stream
	}
	diagStream = isStream

	// For streaming requests, open the Anthropic SSE response (message_start,
	// keepalive ping, content_block_start) BEFORE waiting on the upstream
	// request. Slow reasoning models can take a long time to send their first
	// header; sending the ping now prevents the client's connect->first-byte
	// window from timing out.
	var stream *anthropicStream
	if isStream {
		stream = openAnthropicStream(w, nil, ar.Model)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		logf("relay: messages upstream request failed: %v", err)
		if stream != nil {
			stream.sendError("upstream request failed")
			stream.sendMessageStop()
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	diagUpstream = resp.StatusCode

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		msg := fmt.Sprintf("upstream returned HTTP %d redirect; refused to follow it to protect credentials", resp.StatusCode)
		diagUpErr = msg
		if stream != nil {
			stream.sendError(msg)
			stream.sendMessageStop()
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, msg)
		return
	}
	if resp.StatusCode != http.StatusOK {
		// Surface upstream errors as Anthropic-style envelopes rather than the
		// raw body. If the stream was already opened with a 200, surface the
		// failure as an Anthropic `error` SSE event; otherwise emit a proper
		// {"type":"error",...} JSON preserving the status code.
		body, _ := readAllCloser(resp.Body)
		upErr := extractErrorMessage(body)
		diagUpErr = upErr
		if stream != nil {
			stream.sendError(upErr)
			stream.sendMessageStop()
			return
		}
		// Re-seat the (consumed) body so replaceWithAnthropicError can write the
		// envelope; the extracted message feeds the diagnostics log.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		replaceWithAnthropicError(w, resp)
		return
	}

	if isStream {
		// A 200 with a JSON body (e.g. {"error":"rate limited"}) is not a stream.
		// Translating it silently would produce an empty but "complete" reply,
		// hiding the real failure from the client and the logs.
		if !isEventStream(resp.Header.Get("Content-Type")) {
			body, _ := readAllCloser(resp.Body)
			upErr := extractErrorMessage(body)
			diagUpErr = upErr
			stream.sendError("upstream did not return a stream: " + upErr)
			stream.sendMessageStop()
			return
		}
		stream.readBody(resp)
		return
	}

	raw, err := readAllCloser(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}
	if msg, ok := upstreamErrorPayload(raw); ok {
		diagUpErr = msg
		writeAnthropicError(w, http.StatusBadGateway, msg)
		return
	}
	converted, err := chatCompletionToAnthropic(raw)
	if err != nil {
		diagConvertErr = err.Error()
		logf("messages: convert upstream response failed: %v", err)
		writeAnthropicError(w, http.StatusBadGateway, "failed to convert upstream response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(converted)
}

// anthropicToolCall tracks one streamed tool call from the upstream OpenAI
// delta and the Anthropic content block allocated for it.
type anthropicToolCall struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
	started    bool
}

// anthropicStream carries the shared state for an open Anthropic Messages SSE
// response and the helper used to write and flush each event.
type anthropicStream struct {
	sendEvent    func(event, data string)
	msgID        string
	model        string
	finished     string
	outputTokens int
	inputTokens  int

	// Content block bookkeeping. Anthropic requires content_block_start/stop
	// pairs and sequential indices; a response may interleave one text block
	// with zero or more tool_use blocks.
	nextIndex int
	textIndex int
	textOpen  bool
	tools     map[int]*anthropicToolCall
	toolOrder []int
}

// sendError emits an Anthropic SSE `error` event (never raw Chat Completions
// chunk JSON), used when an already-opened stream must surface a failure.
func (s *anthropicStream) sendError(message string) {
	s.sendEvent("error", anthropicJSON(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	}))
}

func (s *anthropicStream) sendMessageStop() {
	s.sendEvent("message_stop", `{"type":"message_stop"}`)
}

// closeTextBlock closes the currently open text content block, if any.
func (s *anthropicStream) closeTextBlock() {
	if !s.textOpen {
		return
	}
	s.sendEvent("content_block_stop", anthropicJSON(map[string]interface{}{
		"type":  "content_block_stop",
		"index": s.textIndex,
	}))
	s.textOpen = false
}

// emitText opens a text block if needed and emits one text_delta.
func (s *anthropicStream) emitText(text string) {
	if text == "" {
		return
	}
	if !s.textOpen {
		s.textIndex = s.nextIndex
		s.nextIndex++
		s.sendEvent("content_block_start", anthropicJSON(map[string]interface{}{
			"type":  "content_block_start",
			"index": s.textIndex,
			"content_block": map[string]interface{}{
				"type": "text",
				"text": "",
			},
		}))
		s.textOpen = true
	}
	s.outputTokens++
	s.sendEvent("content_block_delta", anthropicJSON(map[string]interface{}{
		"type":  "content_block_delta",
		"index": s.textIndex,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	}))
}

// startTool opens the Anthropic content block for one tool call, flushing any
// arguments buffered before the id and name were known.
func (s *anthropicStream) startTool(tc *anthropicToolCall, args string) {
	tc.blockIndex = s.nextIndex
	s.nextIndex++
	tc.started = true
	s.sendEvent("content_block_start", anthropicJSON(map[string]interface{}{
		"type":  "content_block_start",
		"index": tc.blockIndex,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    tc.id,
			"name":  tc.name,
			"input": map[string]interface{}{},
		},
	}))
	buffered := tc.args.String()
	tc.args.Reset()
	if all := buffered + args; all != "" {
		s.sendEvent("content_block_delta", anthropicJSON(map[string]interface{}{
			"type":  "content_block_delta",
			"index": tc.blockIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": all,
			},
		}))
	}
}

// emitToolCall applies one OpenAI streamed tool_call fragment to the matching
// Anthropic tool_use block, opening it once id and name are known.
func (s *anthropicStream) emitToolCall(index int, id, name, args string) {
	tc := s.tools[index]
	if tc == nil {
		tc = &anthropicToolCall{}
		s.tools[index] = tc
		s.toolOrder = append(s.toolOrder, index)
	}
	if id != "" {
		tc.id = id
	}
	if name != "" {
		tc.name = name
	}
	// Tool blocks follow the text block; close any open text block first.
	s.closeTextBlock()

	if !tc.started {
		if tc.id == "" || tc.name == "" {
			// Arguments can arrive before id/name; buffer until we can open the
			// content_block_start event (Anthropic requires id and name there).
			tc.args.WriteString(args)
			return
		}
		s.startTool(tc, args)
		return
	}
	if args != "" {
		s.sendEvent("content_block_delta", anthropicJSON(map[string]interface{}{
			"type":  "content_block_delta",
			"index": tc.blockIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		}))
	}
}

func (s *anthropicStream) hasToolUse() bool {
	for _, tc := range s.tools {
		if tc.started {
			return true
		}
	}
	return false
}

// readBody consumes the upstream Chat Completions SSE stream and emits the
// remaining Anthropic events (content_block_delta … content_block_stop,
// message_delta, optional error, message_stop). The lifecycle opening events
// (message_start, ping, content_block_start) are emitted by openAnthropicStream
// before any upstream data is available, so this only needs the delta loop.
func (s *anthropicStream) readBody(resp *http.Response) {
	defer resp.Body.Close()

	rd := newSSEReader(resp.Body)
	var streamErr error
	sawDone := false
	sawFinish := false
	for {
		data, err := rd.Next()
		if err != nil {
			if err != io.EOF {
				streamErr = err
			}
			break
		}
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk struct {
			Model string `json:"model"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		// Some providers report failures as an in-band error object on a 200.
		if chunk.Error != nil {
			msg := strings.TrimSpace(chunk.Error.Message)
			if msg == "" {
				msg = "upstream reported an error"
			}
			streamErr = errors.New(msg)
			break
		}

		if chunk.Model != "" {
			s.model = chunk.Model
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				s.inputTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				s.outputTokens = chunk.Usage.CompletionTokens
			}
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				s.emitText(delta.Content)
			}
			for _, tc := range delta.ToolCalls {
				s.emitToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
			if chunk.Choices[0].FinishReason != nil {
				s.finished = anthropicStopReason(*chunk.Choices[0].FinishReason)
				sawFinish = true
			}
		}
	}

	// EOF without [DONE] or a finish_reason means the upstream connection was
	// cut mid-response; report it instead of closing as a complete message.
	if streamErr == nil && !sawDone && !sawFinish {
		streamErr = io.ErrUnexpectedEOF
	}

	// Close every open block before the message_delta summary.
	s.closeTextBlock()
	for _, idx := range s.toolOrder {
		tc := s.tools[idx]
		if tc == nil || !tc.started {
			continue
		}
		s.sendEvent("content_block_stop", anthropicJSON(map[string]interface{}{
			"type":  "content_block_stop",
			"index": tc.blockIndex,
		}))
	}

	stopReason := s.finished
	if s.hasToolUse() {
		stopReason = "tool_use"
	}
	s.sendEvent("message_delta", anthropicJSON(map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": stopReason,
		},
		"usage": map[string]interface{}{
			"input_tokens":  s.inputTokens,
			"output_tokens": s.outputTokens,
		},
	}))
	if streamErr != nil {
		// The upstream stream died before a clean [DONE]: surface an Anthropic
		// `error` SSE event (never raw Chat Completions chunk JSON) so the
		// client can report the failure, then close cleanly with message_stop.
		s.sendError("upstream stream failed: " + streamErr.Error())
	}
	s.sendMessageStop()
}

// openAnthropicStream opens an Anthropic Messages SSE response and immediately
// writes the opening lifecycle events — message_start, a keepalive `ping`, and
// content_block_start — so the client receives its first byte before the
// upstream produces any content. Slow reasoning models can take a long time to
// send their first header, and the ping keeps the client's connect->first-byte
// window from timing out.
func openAnthropicStream(w http.ResponseWriter, src http.Header, model string) *anthropicStream {
	flusher, _ := w.(http.Flusher)
	copyStreamHeaders(w.Header(), src)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}
	s := &anthropicStream{
		msgID:     anthropicMessageID(),
		model:     model,
		finished:  "end_turn",
		nextIndex: 1,
		textIndex: 0,
		textOpen:  true,
		tools:     map[int]*anthropicToolCall{},
	}
	s.sendEvent = func(event, data string) {
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n"); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	s.sendEvent("message_start", anthropicJSON(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}))
	// Keepalive: open the connection immediately for slow-first-header models.
	s.sendEvent("ping", `{"type":"ping"}`)
	s.sendEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	return s
}

// streamChatCompletionToAnthropic converts a Chat Completions SSE stream into
// an Anthropic Messages SSE event stream (message_start … message_stop). It
// opens the stream first, then emits each `content_block_delta` incrementally
// as upstream chunks arrive so the client sees the first output immediately.
func streamChatCompletionToAnthropic(w http.ResponseWriter, resp *http.Response) {
	s := openAnthropicStream(w, resp.Header, "")
	s.readBody(resp)
}

// anthropicJSON marshals v to a compact JSON string for an SSE data payload.
func anthropicJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// byteCountWriter wraps an http.ResponseWriter to count the bytes written out,
// so handleMessages can record the response size in its diagnostics log. It
// forwards Flush so it can also wrap the streaming path, which relies on
// http.Flusher to push SSE events to the client.
type byteCountWriter struct {
	http.ResponseWriter
	n int
}

func (b *byteCountWriter) Write(p []byte) (int, error) {
	n, err := b.ResponseWriter.Write(p)
	b.n += n
	return n, err
}

func (b *byteCountWriter) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logMessagesDiagnostic emits a single structured diagnostics line for one
// /v1/messages request: the model, stream flag, upstream status, the extracted
// upstream error message, conversion-failure detail, and the response byte
// size. The raw API key is intentionally never logged.
func logMessagesDiagnostic(model string, stream bool, upstreamStatus int, upErrMsg string, convertErr string, respBytes int) {
	logf("messages: model=%q stream=%v upstream_status=%d upstream_error=%q convert_error=%q response_bytes=%d",
		model, stream, upstreamStatus, upErrMsg, convertErr, respBytes)
}
