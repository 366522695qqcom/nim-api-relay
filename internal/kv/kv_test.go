package kv

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedisRecordUsesDailyBucketsWithTTL(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// One result per command in the pipeline; contents are unused here.
		w.Write([]byte(`[{"result":1},{"result":1},{"result":1},{"result":1},{"result":1},{"result":1},{"result":1}]`))
	}))
	defer srv.Close()

	store := &redisStore{base: srv.URL, token: "test", client: srv.Client()}
	if err := store.Record(LogEntry{IP: "1.2.3.4", KeyMask: "....abcd"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var cmds [][]interface{}
	if err := json.Unmarshal(body, &cmds); err != nil {
		t.Fatalf("unmarshal commands: %v", err)
	}

	day := time.Now().UTC().Format("20060102")
	var sawIP, sawKey, sawTTL bool
	for _, c := range cmds {
		if len(c) < 3 {
			continue
		}
		cmd, _ := c[0].(string)
		key, _ := c[1].(string)
		switch cmd {
		case "HINCRBY":
			if strings.HasPrefix(key, "relay:byip:") {
				sawIP = true
				if !strings.HasSuffix(key, day) {
					t.Errorf("byip key %q not bucketed by day %s", key, day)
				}
			}
			if strings.HasPrefix(key, "relay:bykey:") {
				sawKey = true
			}
		case "EXPIRE":
			if v, ok := c[2].(float64); ok && int(v) == statsTTLSeconds {
				sawTTL = true
			}
		}
	}
	if !sawIP || !sawKey || !sawTTL {
		t.Errorf("missing daily bucket/TTL commands: byip=%v bykey=%v ttl=%v (%s)", sawIP, sawKey, sawTTL, body)
	}
}

func TestRecordTruncatesLongIP(t *testing.T) {
	e := LogEntry{IP: strings.Repeat("9", 100)}
	if got := sanitizedIP(e.IP); len(got) != 45 {
		t.Errorf("sanitized IP length = %d, want 45", len(got))
	}
}
