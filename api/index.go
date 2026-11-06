package api

import (
	"bytes"
	"encoding/json"
	"errors"
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

// maxBodyBytes caps the request body accepted by the relay.
const maxBodyBytes = 10 << 20 // 10 MiB

// safeResponseHeaders is the whitelist of upstream response headers that may
// reach the client. Everything else (Set-Cookie, CSP, custom headers) is
// dropped so a compromised upstream cannot set cookies or weaken our policy.
var safeResponseHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Cache-Control",
	"Retry-After",
}

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

func upstreamURL() *url.URL {
	raw := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	if raw == "" {
		raw = defaultUpstream
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u, _ = url.Parse(defaultUpstream)
	}
	return u
}

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

var upstreamClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Slow reasoning models (e.g. moonshotai/kimi-k3) can take a long time to
	// send their first response header. Give them ample time to avoid 502s.
	transport.ResponseHeaderTimeout = 300 * time.Second
	return &http.Client{Transport: transport}
}()

// isResponsesPath reports whether the request targets the OpenAI Responses API.
func isResponsesPath(path string) bool {
	p := strings.TrimRight(strings.ToLower(path), "/")
	return p == "/v1/responses" || p == "/responses"
}

func buildUpstreamRequest(target *url.URL, r *http.Request, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, buildUpstreamURL(target, r), body)
	if err != nil {
		return nil, err
	}

	skip := map[string]bool{}
	for _, h := range append(hopByHopHeaders,
		"Accept-Encoding", "Host", "Content-Length",
		"X-Forwarded-For", "X-Real-IP", "Forwarded") {
		skip[http.CanonicalHeaderKey(h)] = true
	}
	for k, vs := range r.Header {
		if skip[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Accept-Encoding", "identity")
	req.Host = target.Host

	// Never trust the inbound X-Forwarded-For chain: a client can prepend
	// arbitrary addresses and poison the upstream's view of the source. Only the
	// address observed by this relay is forwarded.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	req.Header.Set("X-Forwarded-For", host)

	return req, nil
}

func removeHopByHopHeaders(h http.Header) {
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

// copySafeHeaders copies only whitelisted upstream response headers to dst.
func copySafeHeaders(dst, src http.Header) {
	for _, k := range safeResponseHeaders {
		for _, v := range src.Values(k) {
			dst.Add(k, v)
		}
	}
}

func forwardResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	copySafeHeaders(w.Header(), resp.Header)

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

// healthHandler reports relay service health without hitting the upstream.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	payload := map[string]string{
		"status":   "ok",
		"service":  "nim-relay",
		"upstream": upstreamURL().String(),
	}
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// indexHandler renders a landing page.
func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"UTF-8\"><title>已部署</title></head><body><p>您已成功部署本项目</p></body></html>"))
}

// isBodyTooLarge reports whether err came from the request body size limit.
func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// writeBodyTooLarge emits a 413 in the error envelope matching the API family.
func writeBodyTooLarge(w http.ResponseWriter, path string) {
	const msg = "request body too large (limit 10 MiB)"
	if isAnthropicMessagesPath(path) {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, msg)
		return
	}
	writeRelayError(w, http.StatusRequestEntityTooLarge, msg)
}

// Handler is the Vercel serverless entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	// Non-API paths served locally without hitting the upstream.
	switch r.URL.Path {
	case "/", "/index.html":
		indexHandler(w, r)
		return
	case "/health", "/healthz":
		healthHandler(w, r)
		return
	case "/admin", "/admin/":
		adminHandler(w, r)
		return
	}

	// CORS preflight is answered locally; never forwarded upstream.
	if r.Method == http.MethodOptions {
		h := w.Header()
		h.Set("Allow", "GET, HEAD, POST, OPTIONS")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Only read-oriented and completion requests are relayed.
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		writeRelayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Reject oversized bodies before buffering; for chunked bodies the
	// MaxBytesReader below enforces the same limit while streaming.
	if r.ContentLength > maxBodyBytes {
		writeBodyTooLarge(w, r.URL.Path)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// Capture request metadata for the admin stats/logs.
	entry := requestLog{
		ip:      clientIP(r),
		path:    r.URL.Path,
		model:   extractModel(r),
		keyMask: maskKey(r),
		start:   time.Now(),
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		record(entry, sw.status)
	}()
	w = sw

	// Kimi desktop clients talk the OpenAI Responses API. Translate it to the
	// Chat Completions protocol the upstream understands.
	if isResponsesPath(r.URL.Path) {
		handleResponses(w, r)
		return
	}

	// Anthropic SDKs/tools (Claude Code, etc.) speak the Messages API. Translate
	// it to the Chat Completions protocol the upstream understands.
	if isAnthropicMessagesPath(r.URL.Path) {
		handleMessages(w, r)
		return
	}

	// Stream the request body straight to the upstream instead of buffering it
	// first: large payloads used to cost two full copies in memory. Chunked
	// bodies (unknown length) are read once up front so an oversized upload is
	// rejected with 413 before anything is sent upstream.
	var body io.Reader = r.Body
	if r.ContentLength < 0 {
		b, err := readBody(r)
		if err != nil {
			if isBodyTooLarge(err) {
				writeBodyTooLarge(w, r.URL.Path)
				return
			}
			writeRelayError(w, http.StatusBadGateway, "failed to read request body")
			return
		}
		body = bytes.NewReader(b)
		r.ContentLength = int64(len(b))
	}
	req, err := buildUpstreamRequest(upstreamURL(), r, body)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to build upstream request")
		return
	}
	if r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		if isBodyTooLarge(err) {
			writeBodyTooLarge(w, r.URL.Path)
			return
		}
		log.Printf("relay: upstream request failed: %v", err)
		writeRelayError(w, http.StatusBadGateway, "upstream request failed")
		return
	}

	forwardResponse(w, resp)
}
