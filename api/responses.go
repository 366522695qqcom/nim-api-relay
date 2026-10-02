package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// readAllCloser reads a stream to completion and closes it.
func readAllCloser(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}

// trimSlash trims leading and trailing slashes from a path segment.
func trimSlash(s string) string {
	return strings.Trim(s, "/")
}

// copyHeaders copies all headers from src to dst.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyStreamHeaders copies SSE + pass-through headers for a translated stream,
// dropping hop-by-hop headers and any content-length set by the upstream.
func copyStreamHeaders(dst, src http.Header) {
	removeHopByHopHeaders(src)
	for k, vs := range src {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	dst.Set("Content-Type", "text/event-stream")
	dst.Set("Cache-Control", "no-cache")
	dst.Del("Transfer-Encoding")
}

// logf is a thin wrapper over the standard logger used by the relay.
func logf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// responsesRequest carries the OpenAI Responses API request fields that a
// relay needs to translate into a Chat Completions request. Unknown fields are
// intentionally dropped.
type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stop            json.RawMessage `json:"stop"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ResponseFormat  json.RawMessage `json:"response_format"`
}

// normalizeMessageContent converts a Responses content value into a Chat
// Completions-compatible content value. Strings pass through unchanged; arrays
// of content blocks are normalized block-by-block (Responses-only types such as
// input_text / input_image are remapped to text / image_url); unmappable blocks
// are dropped. If the resulting array is empty, an empty string is returned so
// the content does not become null or an empty array.
func normalizeMessageContent(v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		return val
	case []interface{}:
		out := make([]interface{}, 0, len(val))
		for _, block := range val {
			m, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text":
				c := shallowCopyMap(m)
				c["type"] = "text"
				out = append(out, c)
			case "input_image", "input_image_url":
				c := shallowCopyMap(m)
				c["type"] = "image_url"
				if _, hasImageURL := c["image_url"]; !hasImageURL {
					if u, hasURL := c["url"]; hasURL {
						c["image_url"] = u
					}
				}
				out = append(out, c)
			case "text", "image_url":
				out = append(out, m)
			default:
				// Other Responses-only block types cannot be mapped to Chat
				// Completions content blocks, so drop them.
			}
		}
		if len(out) == 0 {
			return ""
		}
		return out
	default:
		return val
	}
}

// shallowCopyMap returns a shallow copy of m so that callers can mutate fields
// (e.g. type) without mutating the original block.
func shallowCopyMap(m map[string]interface{}) map[string]interface{} {
	c := make(map[string]interface{}, len(m))
	for k, val := range m {
		c[k] = val
	}
	return c
}

// responsesInputToMessages converts a Responses `input` value (either a plain
// string or an array of message items) into Chat Completions `messages`.
func responsesInputToMessages(in json.RawMessage) ([]map[string]interface{}, error) {
	if len(bytes.TrimSpace(in)) == 0 || bytes.Equal(bytes.TrimSpace(in), []byte("null")) {
		return nil, nil
	}

	// Plain string input -> single user message.
	var text string
	if err := json.Unmarshal(in, &text); err == nil {
		return []map[string]interface{}{{"role": "user", "content": text}}, nil
	}

	// Array input (conversation items or response messages).
	var arr []json.RawMessage
	if err := json.Unmarshal(in, &arr); err != nil {
		return nil, err
	}

	var out []map[string]interface{}
	for _, item := range arr {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue // skip malformed items rather than fail the whole request
		}
		// Responses input items may carry a `type`; only message items map to
		// chat messages (string or "message"). Drop other item kinds.
		if t, _ := obj["type"].(string); t != "" && t != "message" {
			continue
		}
		role, _ := obj["role"].(string)
		if role == "" {
			role = "user"
		}
		content := obj["content"]
		if content == nil {
			content = ""
		}
		content = normalizeMessageContent(content)
		if role == "developer" {
			role = "system"
		}
		out = append(out, map[string]interface{}{"role": role, "content": content})
	}
	return out, nil
}

// responsesToChat translates a Responses API request body into a Chat
// Completions request body. It returns the JSON body to send upstream.
func responsesToChat(input []byte) ([]byte, error) {
	var rb responsesRequest
	if err := json.Unmarshal(input, &rb); err != nil {
		return nil, err
	}

	chat := map[string]interface{}{"model": rb.Model}

	messages := make([]map[string]interface{}, 0, 4)
	if rb.Instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role": "system", "content": rb.Instructions,
		})
	}
	inputMessages, err := responsesInputToMessages(rb.Input)
	if err != nil {
		return nil, err
	}
	messages = append(messages, inputMessages...)
	chat["messages"] = messages

	if rb.MaxOutputTokens != nil {
		chat["max_tokens"] = *rb.MaxOutputTokens
	}
	if rb.Stream {
		chat["stream"] = true
	}
	if rb.Temperature != nil {
		chat["temperature"] = *rb.Temperature
	}
	if rb.TopP != nil {
		chat["top_p"] = *rb.TopP
	}
	setRawField(chat, "stop", rb.Stop)
	setRawField(chat, "tools", rb.Tools)
	setRawField(chat, "tool_choice", rb.ToolChoice)
	setRawField(chat, "response_format", rb.ResponseFormat)

	return json.Marshal(chat)
}

// setRawField copies a still-encoded JSON value into a target map if non-empty.
func setRawField(target map[string]interface{}, key string, raw json.RawMessage) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err == nil {
		target[key] = v
	}
}

// chatCompletionToResponse converts a non-streaming Chat Completions response
// into a Responses API response object.
func chatCompletionToResponse(in []byte) ([]byte, error) {
	var co struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string            `json:"role"`
				Content          interface{}       `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(in, &co); err != nil {
		return nil, err
	}

	resp := map[string]interface{}{
		"id":         co.ID,
		"object":     "response",
		"created_at": co.Created,
		"model":      co.Model,
		"status":     "completed",
		"output":     []interface{}{},
		"usage": map[string]interface{}{
			"input_tokens":  co.Usage.PromptTokens,
			"output_tokens": co.Usage.CompletionTokens,
			"total_tokens":  co.Usage.TotalTokens,
		},
	}

	if len(co.Choices) == 0 {
		return json.Marshal(resp)
	}

	ch := co.Choices[0]
	switch ch.FinishReason {
	case "length", "max_tokens":
		resp["status"] = "incomplete"
	case "tool_calls", "function_call":
		resp["status"] = "in_progress"
	}

	var output []interface{}

	text := contentToString(ch.Message.Content)
	contentParts := []interface{}{
		map[string]interface{}{
			"type":        "output_text",
			"text":        text,
			"annotations": []interface{}{},
		},
	}

	// Surface reasoning text, if the upstream model provided it.
	if ch.Message.ReasoningContent != "" {
		output = append(output, map[string]interface{}{
			"type":  "function_call",
			"id":    "reasoning_" + co.ID,
			"name":  "reasoning",
			"arguments": fmt.Sprintf(
				`{"summary":[{"type":"summary_text","text":%q}]}`, ch.Message.ReasoningContent),
		})
	}

	itemID := "msg_" + strings.TrimPrefix(co.ID, "chatcmpl-")
	output = append(output, map[string]interface{}{
		"type":   "message",
		"id":     itemID,
		"status": "completed",
		"role":   "assistant",
		"content": contentParts,
	})

	for _, tc := range ch.Message.ToolCalls {
		var call map[string]interface{}
		if err := json.Unmarshal(tc, &call); err != nil {
			continue
		}
		fn, _ := call["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        call["id"],
			"call_id":   call["id"],
			"name":      name,
			"arguments": args,
		})
	}

	resp["output"] = output
	return json.Marshal(resp)
}

// contentToString flattens a Chat Completions content value to its text form.
func contentToString(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" {
				sb.WriteString(stringValue(m["text"]))
			}
		}
		return sb.String()
	default:
		return stringOrJSON(content)
	}
}

func stringValue(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return stringOrJSON(v)
}

func stringOrJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleResponses translates a /v1/responses request into a Chat Completions
// request, forwards it to the upstream, and converts the response back.
func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRelayError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to read request body")
		return
	}

	chatBody, err := responsesToChat(bodyBytes)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid responses request: "+err.Error())
		return
	}

	target := upstreamURL()
	req, err := buildUpstreamRequest(target, r, bytes.NewReader(chatBody))
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to build upstream request")
		return
	}
	req.URL.Path = trimSlash(target.Path) + "/v1/chat/completions"

	resp, err := upstreamClient.Do(req)
	if err != nil {
		logf("relay: responses upstream request failed: %v", err)
		writeRelayError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Pass through upstream errors verbatim.
		removeHopByHopHeaders(resp.Header)
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		body, _ := readAllCloser(resp.Body)
		w.Write(body)
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
		streamChatCompletionToResponses(w, resp)
		return
	}

	raw, err := readAllCloser(resp.Body)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}
	converted, err := chatCompletionToResponse(raw)
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

// streamChatCompletionToResponses converts a Chat Completions SSE stream into a
// Responses API SSE event stream.
func streamChatCompletionToResponses(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	flusher, _ := w.(http.Flusher)
	copyStreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	respID := "resp_" + fmt.Sprintf("%d", time.Now().UnixNano())
	itemID := "item_" + fmt.Sprintf("%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()
	var model string
	var text strings.Builder
	var reasoning strings.Builder

	sendEvent := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Session lifecycle events.
	responseObj := func(status string) string {
		b, _ := json.Marshal(map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"model":      model,
			"output":     []interface{}{},
		})
		return string(b)
	}
	messageItem := func(status, fullText string) string {
		b, _ := json.Marshal(map[string]interface{}{
			"id":     itemID,
			"type":   "message",
			"status": status,
			"role":   "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text", "text": fullText, "annotations": []interface{}{},
			}},
		})
		return string(b)
	}

	sendEvent("response.created", `{"type":"response.created","response":`+responseObj("in_progress")+`}`)
	sendEvent("response.in_progress", `{"type":"response.in_progress","response":`+responseObj("in_progress")+`}`)
	sendEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":`+messageItem("in_progress", "")+`}`)
	sendEvent("response.content_part.added", `{"type":"response.content_part.added","item_id":"`+itemID+`","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)

	buf := bufio.NewReaderSize(resp.Body, 4096)
	finished := ""
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
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emitRaw := `{"type":"response.output_text.delta","delta":` + stringValue(data) + `}`
			sendEvent("response.output_text.delta", emitRaw)
			continue
		}

		if chunk.ID != "" {
			respID = "resp_" + chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.ReasoningContent != "" {
				reasoning.WriteString(delta.ReasoningContent)
				sendEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"`+itemID+`","output_index":0,"content_index":0,"delta":`+stringValue(reasoning.String())+`}`)
			} else if delta.Content != "" {
				text.WriteString(delta.Content)
				sendEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"`+itemID+`","output_index":0,"content_index":0,"delta":`+stringValue(delta.Content)+`}`)
			}
			if chunk.Choices[0].FinishReason != nil {
				finished = *chunk.Choices[0].FinishReason
			}
		}

		if chunk.Usage != nil {
			if finished == "" {
				finished = "stop"
			}
		}
	}

	fullText := text.String()
	sendEvent("response.output_text.done", `{"type":"response.output_text.done","item_id":"`+itemID+`","output_index":0,"content_index":0,"text":`+stringValue(fullText)+`,"annotations":[]}`)
	sendEvent("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":`+messageItem("completed", fullText)+`}`)

	status := "completed"
	if finished == "length" || finished == "max_tokens" {
		status = "incomplete"
	}
	sendEvent("response.completed", `{"type":"response.completed","response":`+responseObj(status)+`}`)
}

// readBody reads and closes the request body.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return readAllCloser(r.Body)
}