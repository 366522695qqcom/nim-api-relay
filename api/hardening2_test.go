package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUpstreamClientDoesNotFollowRedirects(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	req, _ := http.NewRequest(http.MethodPost, redirector.URL, strings.NewReader("{}"))
	req.Header.Set("x-api-key", "nvapi-SECRET")
	resp, err := upstreamClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("redirect target was contacted %d times; credentials leaked", n)
	}
}

func TestHandlerRejectsUpstreamRedirect(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	t.Setenv("UPSTREAM_URL", redirector.URL)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	Handler(rec, r)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("upstream Location leaked: %q", loc)
	}
	if !strings.Contains(rec.Body.String(), "redirect") {
		t.Errorf("expected a redirect explanation, got %s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("redirect target was contacted %d times", n)
	}
}

func TestHealthRedactsUpstreamCredentials(t *testing.T) {
	t.Setenv("UPSTREAM_URL", "http://user:TopSecret@127.0.0.1:9201")
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	Handler(rec, r)

	body := rec.Body.String()
	if strings.Contains(body, "TopSecret") {
		t.Errorf("health leaked the upstream password: %s", body)
	}
	if !strings.Contains(body, "xxxxx") {
		t.Errorf("expected redacted URL, got %s", body)
	}
}

func TestMessagesStreamNonSSEUpstreamErrors(t *testing.T) {
	restore := withStubUpstream(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
		}, nil
	}))
	defer restore()

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	Handler(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected an SSE error event, got:\n%s", body)
	}
	if !strings.Contains(body, "rate limited") {
		t.Errorf("upstream error message was swallowed:\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("stream did not terminate:\n%s", body)
	}
}

func TestMessagesStreamPrematureEOFReportsError(t *testing.T) {
	// A single content delta, then the body ends without finish_reason/[DONE].
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	restore := withStubUpstream(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}))
	defer restore()

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	Handler(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("truncated stream should surface an error event:\n%s", body)
	}
	if !strings.Contains(body, "partial") {
		t.Errorf("partial content should still be delivered:\n%s", body)
	}
}

func TestResponsesStreamPrematureEOFReportsFailure(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	restore := withStubUpstream(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}))
	defer restore()

	r := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	Handler(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Errorf("truncated stream should fail the response:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Errorf("truncated stream must not report completion:\n%s", body)
	}
}

func TestSSEReaderJoinsMultilineData(t *testing.T) {
	// SSE allows one event's payload to span several `data:` lines.
	body := ": keepalive\n\nevent: message\ndata: {\"a\":\ndata: 1}\n\n"
	rd := newSSEReader(strings.NewReader(body))

	got, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "{\"a\":\n1}" {
		t.Errorf("data = %q, want joined multi-line payload", got)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestCopySafeHeadersNeutralizesHTML(t *testing.T) {
	dst := http.Header{}
	copySafeHeaders(dst, http.Header{"Content-Type": {"text/html; charset=utf-8"}})

	if dst.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff: %v", dst)
	}
	if csp := dst.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("upstream HTML not sandboxed: %q", csp)
	}
}

func TestResponsesInputContentConversion(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"input": [{"role": "user", "content": [
			{"type": "input_text", "text": "describe"},
			{"type": "input_image", "image_url": "https://example.com/a.png"}
		]}]
	}`)
	out, err := responsesToChat(in)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat struct {
		Messages []struct {
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parts, ok := chat.Messages[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %v, want text + image_url", chat.Messages[0].Content)
	}
	if parts[0].(map[string]interface{})["type"] != "text" {
		t.Errorf("input_text not converted: %v", parts[0])
	}
	img := parts[1].(map[string]interface{})
	if img["type"] != "image_url" {
		t.Errorf("input_image not converted: %v", img)
	}
}

func TestAnthropicImageBlockConversion(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"max_tokens": 8,
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}},
			{"type": "text", "text": "what is this"}
		]}]
	}`)
	out, err := anthropicToChat(in)
	if err != nil {
		t.Fatalf("anthropicToChat: %v", err)
	}
	var chat struct {
		Messages []struct {
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parts, ok := chat.Messages[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %v, want image_url + text", chat.Messages[0].Content)
	}
	var img map[string]interface{}
	for _, p := range parts {
		if m, ok := p.(map[string]interface{}); ok && m["type"] == "image_url" {
			img = m
		}
	}
	if img == nil {
		t.Fatalf("image block not converted: %v", parts)
	}
	url := img["image_url"].(map[string]interface{})["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image url = %q", url)
	}
}
