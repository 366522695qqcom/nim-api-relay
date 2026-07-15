package relay

import (
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
)

// keyPool manages a set of NVIDIA API keys and provides thread-safe
// round-robin selection across them.
type keyPool struct {
	keys   []string
	offset uint64
}

// newKeyPool reads the NVIDIA_API_KEYS environment variable, parses the
// comma-separated keys, and returns a keyPool ready for round-robin
// selection. It panics if no usable keys are configured, since the relay
// cannot operate without at least one API key.
func newKeyPool() *keyPool {
	raw := os.Getenv("NVIDIA_API_KEYS")
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		panic("relay: NVIDIA_API_KEYS environment variable is not set or contains no valid keys")
	}
	return &keyPool{
		keys:   keys,
		offset: uint64(rand.Intn(len(keys))),
	}
}

// next returns the next API key in round-robin order. It is safe for
// concurrent use by multiple goroutines.
func (p *keyPool) next() string {
	n := uint64(len(p.keys))
	idx := atomic.AddUint64(&p.offset, 1) % n
	return p.keys[idx]
}
