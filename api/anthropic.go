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

	resp, err := upstreamClient.Do(req)
	if err != nil {
		logf("relay: messages upstream request failed: %v", err)
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface upstream errors as Anthropic-style errors, preserving the
		// status code, rather than passing the raw body verbatim.
		replaceWithAnthropicError(w, resp)
		return
	}

	isStream := false
	var p struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(bodyBytes, &p) == nil {
		isStream = p.Stream
	}

	if isStream {
		streamChatCompletionToAnthropic(w, resp)
		return
	}

	raw, err := readAllCloser(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}
	converted, err := chatCompletionToAnthropic(raw)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(converted)
}

// streamChatCompletionToAnthropic converts a Chat Completions SSE stream into
// an Anthropic Messages SSE event stream (message_start … message_stop). It
// emits each event incrementally as upstream chunks arrive (rather than
// buffering the whole stream) so the client sees the first output immediately.
func streamChatCompletionToAnthropic(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	flusher, _ := w.(http.Flusher)
	copyStreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	sendEvent := func(event, data string) {
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n"); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := anthropicMessageID()
	// model may be unknown before the first upstream chunk arrives; start with
	// a placeholder and fill it in as soon as a chunk provides one.
	model := "unknown"
	finished := "end_turn"
	outputTokens := 0

	mustJSON := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	// The lifecycle events are emitted immediately, before reading any upstream
	// chunk, so that the first byte does not wait for the whole response.
	sendEvent("message_start", mustJSON(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}))

	sendEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)

	buf := bufio.NewReaderSize(resp.Body, 4096)
	for {
		line, err := buf.ReadString('\n')
		if err != nil {
			if line == "" {
				break
			}
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
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
			model = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				outputTokens++
				sendEvent("content_block_delta", mustJSON(map[string]interface{}{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]interface{}{
						"type": "text_delta",
						"text": delta.Content,
					},
				}))
			}
			if chunk.Choices[0].FinishReason != nil {
				finished = anthropicStopReason(*chunk.Choices[0].FinishReason)
			}
		}
	}

	sendEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
	sendEvent("message_delta", mustJSON(map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": finished,
		},
		"usage": map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": outputTokens,
		},
	}))
	sendEvent("message_stop", `{"type":"message_stop"}`)
}