package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRejectsOversizedBody(t *testing.T) {
	called := false
	restore := withStubUpstream(roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, io.ErrUnexpectedEOF
	}))
	defer restore()

	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.ContentLength = maxBodyBytes + 1
		rec := httptest.NewRecorder()
		Handler(rec, r)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status = %d, want 413", path, rec.Code)
		}
	}
	if called {
		t.Errorf("oversized request must not reach the upstream")
	}
}

func TestHandlerRejectsUnsupportedMethod(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(method, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		Handler(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("%s: missing Allow header", method)
		}
	}
}

func TestHandlerHandlesPreflight(t *testing.T) {
	r := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	Handler(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("missing CORS headers")
	}
}

func TestCopySafeHeaders(t *testing.T) {
	dst := http.Header{}
	dst.Set("Content-Security-Policy", "frame-ancestors 'none'")
	src := http.Header{
		"Content-Type":            {"application/json"},
		"Content-Length":          {"12"},
		"Cache-Control":           {"no-store"},
		"Retry-After":             {"3"},
		"Set-Cookie":              {"upstream_session=SECRET; Path=/"},
		"Content-Security-Policy": {"default-src 'none'"},
		"X-Upstream-Secret":       {"leak"},
	}
	copySafeHeaders(dst, src)

	for _, want := range []string{"Content-Type", "Content-Length", "Cache-Control", "Retry-After"} {
		if dst.Get(want) == "" {
			t.Errorf("whitelisted header %s missing", want)
		}
	}
	for _, banned := range []string{"Set-Cookie", "X-Upstream-Secret"} {
		if dst.Get(banned) != "" {
			t.Errorf("header %s should have been dropped", banned)
		}
	}
	if got := dst.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("local CSP was overwritten: %q", got)
	}
}

func TestBuildUpstreamRequestIgnoresInboundXFF(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://relay.local/v1/chat/completions", strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	r.Header.Set("X-Real-IP", "8.8.8.8")
	r.Header.Set("Forwarded", "for=9.9.9.9")

	req, err := buildUpstreamRequest(upstreamURL(), r, r.Body)
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	if got := req.Header.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want 127.0.0.1", got)
	}
	if got := req.Header.Get("X-Real-IP"); got != "" {
		t.Errorf("X-Real-IP should be dropped, got %q", got)
	}
}

func TestClientIPUsesLastValidXFFEntry(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4, not-an-ip")
	if got := clientIP(r); got != "1.2.3.4" {
		t.Errorf("clientIP = %q, want 1.2.3.4", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(r); got != "10.0.0.1" {
		t.Errorf("clientIP = %q, want socket address", got)
	}
}

func TestResponsesStreamToolCalls(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","model":"m","choices":[{"delta":{"content":"Checking."},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	rec := httptest.NewRecorder()
	streamChatCompletionToResponses(rec, resp)

	var events []map[string]interface{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err == nil {
			events = append(events, m)
		}
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("unsafe upstream headers leaked: %v", rec.Header())
	}

	added, deltas, done, completed := 0, "", 0, 0
	var completedOutput []interface{}
	for _, ev := range events {
		switch ev["type"] {
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]interface{})
			if item["type"] == "function_call" {
				added++
				if item["name"] != "get_weather" || item["call_id"] != "call_1" {
					t.Errorf("function_call item = %v", item)
				}
			}
		case "response.function_call_arguments.delta":
			deltas += stringValue(ev["delta"])
		case "response.function_call_arguments.done":
			done++
			if ev["arguments"] != `{"city":"Beijing"}` {
				t.Errorf("arguments = %v", ev["arguments"])
			}
		case "response.completed":
			completed++
			r, _ := ev["response"].(map[string]interface{})
			completedOutput, _ = r["output"].([]interface{})
		}
	}
	if added != 1 || done != 1 || completed != 1 {
		t.Errorf("added=%d done=%d completed=%d, want 1/1/1", added, done, completed)
	}
	if deltas != `{"city":"Beijing"}` {
		t.Errorf("accumulated deltas = %q", deltas)
	}
	if len(completedOutput) != 2 {
		t.Errorf("completed output = %v, want message + function_call", completedOutput)
	}
}

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
