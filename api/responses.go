package api

import (
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

// copyStreamHeaders prepares SSE response headers for a translated stream.
// Only safe cache/retry hints are taken from the upstream; everything else
// (cookies, CSP, custom headers) is dropped.
func copyStreamHeaders(dst, src http.Header) {
	removeHopByHopHeaders(src)
	for _, k := range []string{"Retry-After"} {
		for _, v := range src.Values(k) {
			dst.Add(k, v)
		}
	}
	dst.Set("Content-Type", "text/event-stream")
	dst.Set("Cache-Control", "no-cache")
	dst.Set("X-Content-Type-Options", "nosniff")
	dst.Del("Content-Length")
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
		out = append(out, map[string]interface{}{
			"role":    role,
			"content": responsesContentToChat(content),
		})
	}
	return out, nil
}

// responsesContentToChat converts a Responses content array into the Chat
// Completions form. Responses names text parts "input_text"/"output_text" and
// images "input_image", none of which Chat Completions understands.
func responsesContentToChat(content interface{}) interface{} {
	arr, ok := content.([]interface{})
	if !ok {
		return content
	}
	var text strings.Builder
	var parts []interface{}
	for _, p := range arr {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		switch t, _ := m["type"].(string); t {
		case "", "text", "input_text", "output_text":
			text.WriteString(stringField(m, "text"))
		case "input_image", "image_url":
			url := stringField(m, "image_url")
			if url == "" {
				url = stringField(m, "url")
			}
			if url == "" {
				continue // file_id-based images have no Chat Completions equivalent
			}
			parts = append(parts, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": url},
			})
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
	if tools := responsesToolsToChat(rb.Tools); tools != nil {
		chat["tools"] = tools
	}
	if tc := responsesToolChoiceToChat(rb.ToolChoice); tc != nil {
		chat["tool_choice"] = tc
	}
	setRawField(chat, "response_format", rb.ResponseFormat)

	return json.Marshal(chat)
}

// responsesToolsToChat converts Responses API tool definitions into the Chat
// Completions function schema. Responses uses a flat shape
// ({"type":"function","name":…,"parameters":…}) while Chat Completions nests it
// under "function"; passing the flat form through makes the upstream ignore
// the tools entirely. Non-function tools (web_search, file_search, …) have no
// Chat Completions equivalent and are dropped.
func responsesToolsToChat(raw json.RawMessage) interface{} {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var tools []map[string]interface{}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if typ, _ := t["type"].(string); typ != "function" {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]interface{}{"name": name}
		if desc, ok := t["description"]; ok {
			fn["description"] = desc
		}
		if params, ok := t["parameters"]; ok {
			fn["parameters"] = params
		} else {
			fn["parameters"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		if strict, ok := t["strict"]; ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesToolChoiceToChat converts the Responses tool_choice value ("auto",
// "none", "required", or {"type":"function","name":…}) into its Chat
// Completions equivalent.
func responsesToolChoiceToChat(raw json.RawMessage) interface{} {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" && obj.Name != "" {
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": obj.Name},
		}
	}
	return nil
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
	}

	var output []interface{}

	text := contentToString(ch.Message.Content)
	itemID := "msg_" + strings.TrimPrefix(co.ID, "chatcmpl-")
	if text != "" || len(ch.Message.ToolCalls) == 0 {
		output = append(output, map[string]interface{}{
			"type":   "message",
			"id":     itemID,
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "output_text",
					"text":        text,
					"annotations": []interface{}{},
				},
			},
		})
	}

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

// stringField returns m[key] only when it is actually a string. Unlike
// stringValue it never turns a missing value into the literal `null`, which is
// not a usable URL or text.
func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
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
		if isBodyTooLarge(err) {
			writeBodyTooLarge(w, r.URL.Path)
			return
		}
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

	if rejectUpstreamRedirect(w, resp) {
		return
	}

	if resp.StatusCode != http.StatusOK {
		// Normalize upstream failures into our JSON envelope, keeping the status.
		// writeRelayError only sets Content-Type, so preserve the upstream
		// Retry-After first: rate-limited clients rely on it for backoff.
		body, _ := readAllCloser(resp.Body)
		for _, v := range resp.Header.Values("Retry-After") {
			w.Header().Add("Retry-After", v)
		}
		writeRelayError(w, resp.StatusCode, extractErrorMessage(body))
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
		if !isEventStream(resp.Header.Get("Content-Type")) {
			body, _ := readAllCloser(resp.Body)
			writeRelayError(w, http.StatusBadGateway,
				"upstream did not return a stream: "+extractErrorMessage(body))
			return
		}
		streamChatCompletionToResponses(w, resp)
		return
	}

	raw, err := readAllCloser(resp.Body)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}
	if msg, ok := upstreamErrorPayload(raw); ok {
		writeRelayError(w, http.StatusBadGateway, msg)
		return
	}
	converted, err := chatCompletionToResponse(raw)
	if err != nil {
		// Never echo an unconvertible body (often upstream HTML) from our domain.
		writeRelayError(w, http.StatusBadGateway, "upstream returned an invalid response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(converted)
}

// respToolState tracks one streamed tool call and the Responses output item
// allocated for it.
type respToolState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	pending     strings.Builder
	args        strings.Builder
	started     bool
}

// streamChatCompletionToResponses converts a Chat Completions SSE stream into a
// Responses API SSE event stream, including function_call items for streamed
// tool calls.
func streamChatCompletionToResponses(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	flusher, _ := w.(http.Flusher)
	copyStreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	respID := "resp_" + fmt.Sprintf("%d", time.Now().UnixNano())
	messageItemID := "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()

	var model string
	var text strings.Builder
	var inputTokens, outputTokens int
	finished := ""

	// Output items are assigned indices in the order they first appear.
	nextIndex := 0
	textIndex := -1
	textOpen := false
	tools := map[int]*respToolState{}
	toolOrder := []int{}

	emit := func(event string, payload map[string]interface{}) {
		payload["type"] = event
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	responseObj := func(status string, output []interface{}) map[string]interface{} {
		return map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"model":      model,
			"output":     output,
			"usage": map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"total_tokens":  inputTokens + outputTokens,
			},
		}
	}

	messageItem := func(status, fullText string) map[string]interface{} {
		return map[string]interface{}{
			"id":     messageItemID,
			"type":   "message",
			"status": status,
			"role":   "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text", "text": fullText, "annotations": []interface{}{},
			}},
		}
	}

	startText := func() {
		if textOpen {
			return
		}
		textOpen = true
		textIndex = nextIndex
		nextIndex++
		emit("response.output_item.added", map[string]interface{}{
			"output_index": textIndex,
			"item":         messageItem("in_progress", ""),
		})
		emit("response.content_part.added", map[string]interface{}{
			"item_id":       messageItemID,
			"output_index":  textIndex,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
	}

	emitTextDelta := func(delta string) {
		startText()
		text.WriteString(delta)
		emit("response.output_text.delta", map[string]interface{}{
			"item_id":       messageItemID,
			"output_index":  textIndex,
			"content_index": 0,
			"delta":         delta,
		})
	}

	// addTool opens the function_call output item once id and name are known.
	addTool := func(st *respToolState) bool {
		if st.started || st.name == "" || st.callID == "" {
			return st.started
		}
		st.outputIndex = nextIndex
		nextIndex++
		st.started = true
		if st.itemID == "" {
			st.itemID = "fc_" + st.callID
		}
		emit("response.output_item.added", map[string]interface{}{
			"output_index": st.outputIndex,
			"item": map[string]interface{}{
				"id":        st.itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   st.callID,
				"name":      st.name,
				"arguments": "",
			},
		})
		return true
	}

	emitArgs := func(st *respToolState, delta string) {
		if delta == "" {
			return
		}
		st.args.WriteString(delta)
		emit("response.function_call_arguments.delta", map[string]interface{}{
			"item_id":      st.itemID,
			"output_index": st.outputIndex,
			"delta":        delta,
		})
	}

	emit("response.created", map[string]interface{}{
		"response": responseObj("in_progress", []interface{}{}),
	})
	emit("response.in_progress", map[string]interface{}{
		"response": responseObj("in_progress", []interface{}{}),
	})

	rd := newSSEReader(resp.Body)
	streamErr := ""
	sawDone := false
	for {
		data, err := rd.Next()
		if err != nil {
			if err != io.EOF {
				streamErr = err.Error()
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
			ID    string `json:"id"`
			Model string `json:"model"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
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
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			msg := strings.TrimSpace(chunk.Error.Message)
			if msg == "" {
				msg = "upstream reported an error"
			}
			streamErr = msg
			break
		}

		if chunk.ID != "" {
			respID = "resp_" + chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				inputTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				outputTokens = chunk.Usage.CompletionTokens
			}
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.ReasoningContent != "" {
				emitTextDelta(delta.ReasoningContent)
			}
			if delta.Content != "" {
				emitTextDelta(delta.Content)
			}
			for _, tc := range delta.ToolCalls {
				st := tools[tc.Index]
				if st == nil {
					st = &respToolState{}
					tools[tc.Index] = st
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					st.callID = tc.ID
				}
				if tc.Function.Name != "" {
					st.name = tc.Function.Name
				}
				wasStarted := st.started
				if addTool(st) {
					if !wasStarted {
						// Flush arguments buffered before id/name arrived.
						if pending := st.pending.String(); pending != "" {
							st.pending.Reset()
							emitArgs(st, pending)
						}
					}
					emitArgs(st, tc.Function.Arguments)
				} else {
					// Arguments can precede id/name; buffer until the item opens.
					st.pending.WriteString(tc.Function.Arguments)
				}
			}
			if chunk.Choices[0].FinishReason != nil {
				finished = *chunk.Choices[0].FinishReason
				sawDone = true
			}
		}
	}
	// EOF without [DONE] or a finish_reason means the upstream connection was
	// cut mid-response; fail the response instead of reporting completion.
	if streamErr == "" && !sawDone {
		streamErr = io.ErrUnexpectedEOF.Error()
	}

	// Close open items in output order.
	var output []interface{}
	if textOpen {
		emit("response.output_text.done", map[string]interface{}{
			"item_id":       messageItemID,
			"output_index":  textIndex,
			"content_index": 0,
			"text":          text.String(),
			"annotations":   []interface{}{},
		})
		item := messageItem("completed", text.String())
		emit("response.output_item.done", map[string]interface{}{
			"output_index": textIndex,
			"item":         item,
		})
		output = append(output, item)
	}
	for _, idx := range toolOrder {
		st := tools[idx]
		if st == nil {
			continue
		}
		if !st.started {
			// Never saw both id and name; there is no item to attach the
			// buffered arguments to.
			continue
		}
		emit("response.function_call_arguments.done", map[string]interface{}{
			"item_id":      st.itemID,
			"output_index": st.outputIndex,
			"arguments":    st.args.String(),
		})
		item := map[string]interface{}{
			"id":        st.itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   st.callID,
			"name":      st.name,
			"arguments": st.args.String(),
		}
		emit("response.output_item.done", map[string]interface{}{
			"output_index": st.outputIndex,
			"item":         item,
		})
		output = append(output, item)
	}

	status := "completed"
	if finished == "length" || finished == "max_tokens" {
		status = "incomplete"
	}
	if streamErr != "" {
		emit("response.failed", map[string]interface{}{
			"response": responseObj("failed", output),
		})
		return
	}
	emit("response.completed", map[string]interface{}{
		"response": responseObj(status, output),
	})
}

// readBody reads and closes the request body.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return readAllCloser(r.Body)
}
