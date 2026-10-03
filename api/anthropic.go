package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
// an array of blocks) into a plain chat content string. Text blocks are
// concatenated; image blocks are passed through for multimodal support.
func anthropicContentToChat(content interface{}) interface{} {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var text strings.Builder
		var parts []interface{}
		hasNonText := false
		for _, p := range v {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			switch t, _ := m["type"].(string); t {
			case "", "text":
				text.WriteString(stringValue(m["text"]))
			case "image", "image_url":
				hasNonText = true
				parts = append(parts, m)
			}
		}
		if hasNonText && text.Len() > 0 {
			parts = append([]interface{}{
				map[string]interface{}{"type": "text", "text": text.String()},
			}, parts...)
		}
		if hasNonText {
			return parts
		}
		return text.String()
	default:
		return content
	}
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
		out = append(out, map[string]interface{}{
			"role":    m.Role,
			"content": anthropicContentToChat(m.Content),
		})
	}
	return out, nil
}

// anthropicToChat translates an Anthropic Messages request body into a Chat
// Completions request body.
func anthropicToChat(input []byte) ([]byte, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(input, &ar); err != nil {
		return nil, err
	}

	chat := map[string]interface{}{"model": ar.Model}

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
	setRawField(chat, "tools", ar.Tools)
	setRawField(chat, "tool_choice", ar.ToolChoice)
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
		"type":  "message",
		"id":    "msg_" + strings.TrimPrefix(co.ID, "chatcmpl-"),
		"model": co.Model,
		"role":  "assistant",
		"content": content,
		"stop_reason":    stopReason,
		"stop_sequence":  nil,
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
		stream = openAnthropicStream(w, nil)
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
		stream.readBody(resp)
		return
	}

	raw, err := readAllCloser(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
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

// anthropicStream carries the shared state for an open Anthropic Messages SSE
// response and the helper used to write and flush each event.
type anthropicStream struct {
	sendEvent    func(event, data string)
	msgID        string
	model        string
	finished     string
	outputTokens int
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

// readBody consumes the upstream Chat Completions SSE stream and emits the
// remaining Anthropic events (content_block_delta … content_block_stop,
// message_delta, optional error, message_stop). The lifecycle opening events
// (message_start, ping, content_block_start) are emitted by openAnthropicStream
// before any upstream data is available, so this only needs the delta loop.
func (s *anthropicStream) readBody(resp *http.Response) {
	defer resp.Body.Close()

	buf := bufio.NewReaderSize(resp.Body, 4096)
	var streamErr error
	for {
		line, err := buf.ReadString('\n')
		hard := err != nil && err != io.EOF
		if err != nil {
			if line == "" {
				// Clean end of the upstream stream (EOF) — unless the error is a
				// hard (non-EOF) failure, in which case this is an abnormal stop.
				if hard {
					streamErr = err
				}
				break
			}
			// Leftover partial line alongside the error: remember a hard failure
			// but still process the trailing line below.
			if hard {
				streamErr = err
			}
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			// Stream closed normally.
			streamErr = nil
			break
		}

		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		if chunk.Model != "" {
			s.model = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				s.outputTokens++
				s.sendEvent("content_block_delta", anthropicJSON(map[string]interface{}{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]interface{}{
						"type": "text_delta",
						"text": delta.Content,
					},
				}))
			}
			if chunk.Choices[0].FinishReason != nil {
				s.finished = anthropicStopReason(*chunk.Choices[0].FinishReason)
			}
		}
	}

	s.sendEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
	s.sendEvent("message_delta", anthropicJSON(map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": s.finished,
		},
		"usage": map[string]interface{}{
			"input_tokens":  0,
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
func openAnthropicStream(w http.ResponseWriter, src http.Header) *anthropicStream {
	flusher, _ := w.(http.Flusher)
	copyStreamHeaders(w.Header(), src)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	s := &anthropicStream{
		msgID:    anthropicMessageID(),
		model:    "unknown",
		finished: "end_turn",
	}
	// model may be unknown before the first upstream chunk arrives; start with
	// a placeholder and fill it in as soon as a chunk provides one.
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
	s := openAnthropicStream(w, resp.Header)
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