package api

import (
	"encoding/json"
	"os"
	"strings"
)

// sanitizeChatRequestFields strips disallowed top-level request fields from a
// Chat Completions request body. NVIDIA returns 400 when stream_options is
// present while stream is not true, so stream_options is only kept when the
// top-level stream value is a boolean true. Fields in strip are always removed.
// It is a pure function: it never mutates its input. An empty body, or a body
// that is not a JSON object, is returned unchanged.
func sanitizeChatRequestFields(body []byte, strip []string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, nil
	}
	if obj == nil {
		return body, nil
	}

	stream, _ := obj["stream"].(bool)
	if !stream {
		delete(obj, "stream_options")
	}

	for _, key := range strip {
		delete(obj, key)
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sanitizeChatRequest is a wrapper around sanitizeChatRequestFields that reads
// the comma-separated STRIP_FIELDS environment variable to decide which
// top-level fields to strip. Whitespace around each field is trimmed and empty
// entries are ignored.
func sanitizeChatRequest(body []byte) ([]byte, error) {
	var strip []string
	for _, f := range strings.Split(os.Getenv("STRIP_FIELDS"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			strip = append(strip, f)
		}
	}
	return sanitizeChatRequestFields(body, strip)
}