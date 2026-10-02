package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeChatRequestFields_StreamOptions_StreamFalse(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"stream": false,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := sanitizeChatRequestFields(in, nil)
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	if strings.Contains(string(out), "stream_options") {
		t.Errorf("stream_options should be dropped when stream is false: %s", out)
	}
}

func TestSanitizeChatRequestFields_StreamOptions_StreamTrue(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := sanitizeChatRequestFields(in, nil)
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	if !strings.Contains(string(out), "stream_options") {
		t.Errorf("stream_options should be kept when stream is true: %s", out)
	}
}

func TestSanitizeChatRequestFields_StripFieldRemoved(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"metadata": {"foo": "bar"},
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := sanitizeChatRequestFields(in, []string{"metadata"})
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := obj["metadata"]; ok {
		t.Errorf("metadata should be stripped: %s", out)
	}
	if obj["model"] != "m" {
		t.Errorf("model lost: %v", obj["model"])
	}
}

func TestSanitizeChatRequestFields_AcceptedFieldsPreserved(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"messages": [{"role": "user", "content": "hi"}],
		"max_tokens": 100,
		"temperature": 0.5,
		"top_p": 0.9,
		"stream": false,
		"stop": ["\n\n"],
		"tools": [{"type": "function", "function": {"name": "f", "parameters": {}}}],
		"tool_choice": "auto",
		"reasoning_effort": "high",
		"response_format": {"type": "json_object"}
	}`)
	out, err := sanitizeChatRequestFields(in, []string{"metadata"})
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"model", "messages", "max_tokens", "temperature", "top_p", "stream", "stop", "tools", "tool_choice", "reasoning_effort", "response_format"} {
		if _, ok := obj[key]; !ok {
			t.Errorf("field %q should be preserved: %s", key, out)
		}
	}
}

func TestSanitizeChatRequestFields_EmptyBody(t *testing.T) {
	out, err := sanitizeChatRequestFields(nil, []string{"metadata"})
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty body should be returned unchanged, got %q", out)
	}
}

func TestSanitizeChatRequestFields_InvalidJSON(t *testing.T) {
	in := []byte(`{"model": `)
	out, err := sanitizeChatRequestFields(in, []string{"metadata"})
	if err != nil {
		t.Fatalf("sanitizeChatRequestFields: %v", err)
	}
	if !strings.EqualFold(string(out), string(in)) {
		t.Errorf("invalid JSON should pass through unchanged: %q", out)
	}
}