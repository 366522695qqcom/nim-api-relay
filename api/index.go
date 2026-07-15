package api

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
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport}
}()

func buildUpstreamRequest(target *url.URL, r *http.Request, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, buildUpstreamURL(target, r), body)
	if err != nil {
		return nil, err
	}

	skip := map[string]bool{
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

	req.Header.Set("Accept-Encoding", "identity")
	req.Host = target.Host

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			host = prior + ", " + host
		}
		req.Header.Set("X-Forwarded-For", host)
	}

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

// Handler is the Vercel serverless entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	target := upstreamURL()

	var bodyBytes []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			writeRelayError(w, http.StatusBadGateway, "failed to read request body")
			return
		}
		bodyBytes = b
	}

	var body io.Reader
	if len(bodyBytes) > 0 {
		body = bytes.NewReader(bodyBytes)
	}

	req, err := buildUpstreamRequest(target, r, body)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "failed to build upstream request")
		return
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		log.Printf("relay: upstream request failed: %v", err)
		writeRelayError(w, http.StatusBadGateway, "upstream request failed")
		return
	}

	forwardResponse(w, resp)
}
