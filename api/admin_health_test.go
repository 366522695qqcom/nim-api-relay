package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nim-relay/internal/kv"
)

// failingStore simulates a configured-but-broken KV backend.
type failingStore struct{}

func (failingStore) Configured() bool { return true }
func (failingStore) Record(kv.LogEntry) error {
	return errors.New("kv unavailable")
}
func (failingStore) Stats() (kv.Stats, []kv.LogEntry, error) {
	return kv.Stats{}, nil, errors.New("dial tcp: connection refused")
}

func TestAdminShowsStoreFailure(t *testing.T) {
	orig := kvStore
	kvStore = failingStore{}
	defer func() { kvStore = orig }()

	t.Setenv("ADMIN_PASSWORD", "pw")
	r := httptest.NewRequest(http.MethodGet, "/admin?token=pw", nil)
	rec := httptest.NewRecorder()
	adminHandler(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "存储异常") {
		t.Errorf("panel should flag a storage failure:\n%s", body[:min(len(body), 400)])
	}
	if !strings.Contains(body, "connection refused") {
		t.Errorf("panel should surface the KV error detail")
	}
	if strings.Contains(body, "● 正常") {
		t.Errorf("panel must not claim health while stats are failing")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
