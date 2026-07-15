// Package relay implements a reverse proxy that forwards requests to an
// upstream NVIDIA NIM API endpoint with automatic API-key rotation on
// authentication (401) or rate-limit (429) failures.
package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// defaultUpstream is used when the UPSTREAM_URL environment variable is not set.
const defaultUpstream = "https://integrate.api.nvidia.com"

// hopByHopHeaders are headers specific to a single transport-level connection.
// They must not be forwarded by a proxy, as defined by RFC 7230.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// upstreamURL resolves the target upstream URL from the UPSTREAM_URL
// environment variable, falling back to the default NVIDIA NIM endpoint.
func upstreamURL() *url.URL {
	raw := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	if raw == "" {
		raw = defaultUpstream
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("relay: invalid UPSTREAM_URL %q, falling back to %s", raw, defaultUpstream)
		u, _ = url.Parse(defaultUpstream)
	}
	return u
}

// buildUpstreamURL constructs the absolute upstream URL for an incoming
// request, preserving the original path and query string and prepending any
// base path configured on the target.
func buildUpstreamURL(target *url.URL, r *http.Request) string {
	path := r.URL.Path
	if target.Path != "" && target.Path != "/" {
		path = strings.TrimRight(target.Path, "/") + path
	}
	u := &url.URL{
		Scheme: target.Scheme,
		Host:   target.Host,
		Path:   path,
	}
	if r.URL.RawQuery != "" {
		u.RawQuery = r.URL.RawQuery
	}
	return u.String()
}

// upstreamClient is the shared HTTP client used to dispatch requests to the
// upstream. It deliberately omits http.Client.Timeout because streaming
// responses (SSE) may legitimately remain open for a long time. Instead, the
// transport bounds only the time we wait for response headers via
// ResponseHeaderTimeout, so a stuck upstream is detected without aborting
// healthy long-running streams.
var upstreamClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport}
}()

// buildUpstreamRequest constructs an outgoing *http.Request addressed to the
// upstream, with the supplied API key and a fresh reader over the cached
// request body. The incoming request is not mutated, so it can be safely
// replayed across multiple key-rotation retries.
func buildUpstreamRequest(target *url.URL, r *http.Request, key string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, buildUpstreamURL(target, r), body)
	if err != nil {
		return nil, err
	}

	// Copy client headers, skipping hop-by-hop headers, Authorization (which
	// we replace with the pool key), Accept-Encoding (which we force to
	// identity), Host (set via req.Host), and Content-Length (derived from
	// the body reader by the http package).
	skip := map[string]bool{
		"Authorization":   true,
		"Accept-Encoding": true,
		"Host":            true,
		"Content-Length":  true,
	}
	for _, h := range hopByHopHeaders {
		skip[h] = true
	}
	for k, vs := range r.Header {
		if skip[k] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	// Replace the Authorization header with a bearer token from the pool.
	req.Header.Set("Authorization", "Bearer "+key)

	// Force identity encoding so SSE streams are not gzip-compressed by the
	// upstream, allowing us to forward each chunk verbatim.
	req.Header.Set("Accept-Encoding", "identity")

	// Route to the upstream host.
	req.Host = target.Host

	// Maintain the X-Forwarded-For chain so the upstream has visibility into
	// the original requester.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			host = prior + ", " + host
		}
		req.Header.Set("X-Forwarded-For", host)
	}

	return req, nil
}

// removeHopByHopHeaders deletes all hop-by-hop headers from h, including any
// headers advertised via the Connection header.
func removeHopByHopHeaders(h http.Header) {
	// Headers listed in Connection are also hop-by-hop; capture them before
	// we delete the Connection header itself.
	var connectionHeaders []string
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				connectionHeaders = append(connectionHeaders, f)
			}
		}
	}
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
	for _, k := range connectionHeaders {
		h.Del(k)
	}
}

// forwardResponse streams an upstream response back to the client, copying
// the status code and headers (minus hop-by-hop) and flushing after every
// write so SSE chunks are delivered immediately. The response body is always
// closed before returning.
func forwardResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// writeRelayError writes a JSON error response consistent with the relay's
// error envelope: {"error":{"message":...,"type":"relay_error"}}.
func writeRelayError(w http.ResponseWriter, status int, message string) {
	payload := map[string]map[string]string{
		"error": {
			"message": message,
			"type":    "relay_error",
		},
	}
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// NewHandler returns an http.Handler that proxies incoming requests to the
// configured upstream NVIDIA NIM API. On 401 or 429 responses it rotates to
// the next API key and retries, up to one attempt per configured key. If all
// keys are exhausted (or every upstream request fails due to a network
// error), the handler responds with 502 and a JSON error body.
//
// The returned handler is shared by the local main entry and the Vercel
// serverless handler.
func NewHandler() http.Handler {
	target := upstreamURL()
	pool := newKeyPool()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cache the request body so it can be replayed across key retries.
		// r.Body is non-nil for server requests, but we guard against nil
		// defensively.
		var bodyBytes []byte
		if r.Body != nil {
			b, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				log.Printf("relay: read request body for %s %s: %v", r.Method, r.URL.Path, err)
				writeRelayError(w, http.StatusBadGateway, "failed to read request body")
				return
			}
			bodyBytes = b
		}

		attempts := len(pool.keys)
		for i := 0; i < attempts; i++ {
			key := pool.next()

			var body io.Reader
			if len(bodyBytes) > 0 {
				body = bytes.NewReader(bodyBytes)
			}

			req, err := buildUpstreamRequest(target, r, key, body)
			if err != nil {
				log.Printf("relay: build upstream request for %s %s: %v", r.Method, r.URL.Path, err)
				writeRelayError(w, http.StatusBadGateway, "failed to build upstream request")
				return
			}

			resp, err := upstreamClient.Do(req)
			if err != nil {
				// Network-level failure (DNS, connection refused, TLS error,
				// client disconnect, etc.): rotate to the next key.
				log.Printf("relay: upstream request %d/%d for %s %s failed: %v", i+1, attempts, r.Method, r.URL.Path, err)
				continue
			}

			// 401/429 indicates the key is invalid or rate-limited: close
			// the body and try the next key.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
				resp.Body.Close()
				log.Printf("relay: upstream returned %d for %s %s (attempt %d/%d), rotating key", resp.StatusCode, r.Method, r.URL.Path, i+1, attempts)
				continue
			}

			// Any other status code (including non-2xx like 400, 404, 500)
			// is forwarded verbatim to the client. forwardResponse closes
			// the response body.
			forwardResponse(w, resp)
			return
		}

		// All keys exhausted.
		log.Printf("relay: all %d API keys exhausted for %s %s", attempts, r.Method, r.URL.Path)
		writeRelayError(w, http.StatusBadGateway, "all API keys exhausted")
	})
}
