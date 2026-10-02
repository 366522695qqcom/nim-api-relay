// Package kv provides a minimal persistent key-value store used for the admin
// page's request logs and statistics. It talks to Vercel KV (Upstash Redis)
// over its REST API so it works on Serverless where no persistent connections
// are possible. If the KV credentials are not configured it degrades to an
// in-process memory store (useful for local development and as a fallback).
package kv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// LogEntry is a single recorded request.
type LogEntry struct {
	Time    string `json:"time"`
	IP      string `json:"ip"`
	Path    string `json:"path"`
	Model   string `json:"model"`
	KeyMask string `json:"key"`
	Status  int    `json:"status"`
	Ms      int64  `json:"ms"`
}

// Stats aggregates total count plus per-IP and per-Key counts.
type Stats struct {
	Total int64
	ByIP  map[string]int64
	ByKey map[string]int64
}

const maxLogs = 200

// Store is the interface implemented by both the Redis and memory backends.
type Store interface {
	Configured() bool
	Record(LogEntry) error
	Stats() (Stats, []LogEntry, error)
}

// New returns a Store backed by Upstash Redis when KV_REST_API_URL and
// KV_REST_API_TOKEN are present, otherwise an in-memory store.
func New() Store {
	url := strings.TrimSpace(os.Getenv("KV_REST_API_URL"))
	token := strings.TrimSpace(os.Getenv("KV_REST_API_TOKEN"))
	if url != "" && token != "" {
		return &redisStore{
			base:  strings.TrimRight(url, "/"),
			token: token,
			client: &http.Client{
				Timeout: 5 * time.Second,
			},
		}
	}
	return newMemStore()
}

// redisStore speaks the Upstash Redis REST protocol:
//   - single command:  POST {base}/{COMMAND}/args...?body=...
//   - pipeline:        POST {base}/pipeline  with a JSON array of commands
type redisStore struct {
	base   string
	token  string
	client *http.Client
}

func (s *redisStore) Configured() bool { return true }

// pipeline sends an array of Redis commands in one HTTP call.
func (s *redisStore) pipeline(cmds [][]interface{}) error {
	if len(cmds) == 0 {
		return nil
	}
	body, err := json.Marshal(cmds)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, s.base+"/pipeline", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("pipeline status %d", resp.StatusCode)
	}
	// Drain; individual errors are not raised so recording never blocks relay.
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *redisStore) Record(e LogEntry) error {
	entry, err := json.Marshal(e)
	if err != nil {
		return err
	}
	cmds := [][]interface{}{
		{"INCR", "relay:total"},
		{"HINCRBY", "relay:byip", e.IP, 1},
	}
	if e.KeyMask != "" {
		cmds = append(cmds, []interface{}{"HINCRBY", "relay:bykey", e.KeyMask, 1})
	}
	cmds = append(cmds,
		[]interface{}{"LPUSH", "relay:logs", string(entry)},
		[]interface{}{"LTRIM", "relay:logs", 0, maxLogs - 1},
	)
	return s.pipeline(cmds)
}

// cmd runs a single command and decodes the Upstash {"result": ...} envelope
// into out.
func (s *redisStore) cmd(name string, args []string, out interface{}) error {
	parts := []string{s.base}
	parts = append(parts, strings.ToUpper(name))
	parts = append(parts, args...)
	u := strings.Join(parts, "/")
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Error != "" {
		return fmt.Errorf("%s: %s", name, envelope.Error)
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

func (s *redisStore) Stats() (Stats, []LogEntry, error) {
	st := Stats{
		ByIP:  map[string]int64{},
		ByKey: map[string]int64{},
	}

	var total float64
	if err := s.cmd("GET", []string{"relay:total"}, &total); err != nil {
		return st, nil, err
	}
	st.Total = int64(total)

	var flat []string
	if err := s.cmd("HGETALL", []string{"relay:byip"}, &flat); err == nil {
		for i := 0; i+1 < len(flat); i += 2 {
			var v float64
			_ = json.Unmarshal([]byte(flat[i+1]), &v)
			st.ByIP[flat[i]] = int64(v)
		}
	}
	if err := s.cmd("HGETALL", []string{"relay:bykey"}, &flat); err == nil {
		for i := 0; i+1 < len(flat); i += 2 {
			var v float64
			_ = json.Unmarshal([]byte(flat[i+1]), &v)
			st.ByKey[flat[i]] = int64(v)
		}
	}

	var raw []string
	if err := s.cmd("LRANGE", []string{"relay:logs", "0", "199"}, &raw); err != nil {
		return st, nil, err
	}
	logs := make([]LogEntry, 0, len(raw))
	for _, line := range raw {
		var e LogEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			logs = append(logs, e)
		}
	}
	return st, logs, nil
}

// memStore is a process-local fallback used when KV is not configured. On
// Serverless each invocation is a fresh process, so this only accumulates
// within a single execution.
type memStore struct {
	mu    sync.Mutex
	total int64
	byIP  map[string]int64
	byKey map[string]int64
	logs  []LogEntry
}

func newMemStore() *memStore {
	return &memStore{
		byIP:  map[string]int64{},
		byKey: map[string]int64{},
		logs:  make([]LogEntry, 0, maxLogs),
	}
}

func (m *memStore) Configured() bool { return false }

func (m *memStore) Record(e LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total++
	m.byIP[e.IP]++
	if e.KeyMask != "" {
		m.byKey[e.KeyMask]++
	}
	m.logs = append([]LogEntry{e}, m.logs...)
	if len(m.logs) > maxLogs {
		m.logs = m.logs[:maxLogs]
	}
	return nil
}

func (m *memStore) Stats() (Stats, []LogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byIP := make(map[string]int64, len(m.byIP))
	for k, v := range m.byIP {
		byIP[k] = v
	}
	byKey := make(map[string]int64, len(m.byKey))
	for k, v := range m.byKey {
		byKey[k] = v
	}
	logs := make([]LogEntry, len(m.logs))
	copy(logs, m.logs)
	return Stats{Total: m.total, ByIP: byIP, ByKey: byKey}, logs, nil
}