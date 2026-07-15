package relay

import (
	"os"
	"testing"
)

// 测试逗号分隔解析（含空格处理）
func TestKeyPoolParsing(t *testing.T) {
	os.Setenv("NVIDIA_API_KEYS", "key1, key2 , key3")
	defer os.Unsetenv("NVIDIA_API_KEYS")

	pool := newKeyPool()
	if len(pool.keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(pool.keys))
	}
	if pool.keys[0] != "key1" || pool.keys[1] != "key2" || pool.keys[2] != "key3" {
		t.Fatalf("unexpected keys: %v", pool.keys)
	}
}

// 测试 round-robin 均衡分布
func TestKeyPoolRoundRobin(t *testing.T) {
	os.Setenv("NVIDIA_API_KEYS", "key1,key2,key3")
	defer os.Unsetenv("NVIDIA_API_KEYS")

	pool := newKeyPool()
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		counts[pool.next()]++
	}
	for _, k := range []string{"key1", "key2", "key3"} {
		if counts[k] != 2 {
			t.Errorf("key %s used %d times, expected 2", k, counts[k])
		}
	}
}

// 测试单 Key 模式
func TestKeyPoolSingleKey(t *testing.T) {
	os.Setenv("NVIDIA_API_KEYS", "only-key")
	defer os.Unsetenv("NVIDIA_API_KEYS")

	pool := newKeyPool()
	for i := 0; i < 5; i++ {
		if pool.next() != "only-key" {
			t.Error("expected only-key")
		}
	}
}

// 测试无 Key 时 panic
func TestKeyPoolNoKeys(t *testing.T) {
	os.Unsetenv("NVIDIA_API_KEYS")
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when no keys configured")
		}
	}()
	newKeyPool()
}
