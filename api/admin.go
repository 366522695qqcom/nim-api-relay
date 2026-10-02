package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"nim-relay/internal/kv"
)

// kvStore is the shared store used to record and read request stats/logs.
var kvStore = kv.New()

// requestLog carries the info captured for one relayed request.
type requestLog struct {
	ip      string
	path    string
	model   string
	keyMask string
	start   time.Time
}

// clientIP returns the best-effort client IP: the first X-Forwarded-For entry
// (set by Vercel/CDN) falling back to the socket address.
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i != -1 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.TrimSpace(host)
}

// extractModel lazily reads a "model" field from the request body without
// consuming the body, restoring it for downstream handling.
func extractModel(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, _ := readBody(r)
	if len(body) == 0 {
		return ""
	}
	// Restore the body so downstream reading still sees it.
	r.Body = newReadCloser(body)
	var p struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &p) == nil {
		return strings.TrimSpace(p.Model)
	}
	return ""
}

// maskKey redacts an Authorization bearer key to a stable hash + last 4 chars
// so the full key is never stored or displayed.
func maskKey(r *http.Request) string {
	aut := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(aut, "Bearer ") {
		return ""
	}
	key := strings.TrimSpace(aut[len("Bearer "):])
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])[:8]
	tail := key
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return "sk-…" + tail + " [" + h + "]"
}

func newReadCloser(b []byte) *readCloser {
	return &readCloser{b: b}
}

type readCloser struct {
	b []byte
	i int
}

func (r *readCloser) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func (r *readCloser) Close() error { return nil }

// statusWriter wraps an http.ResponseWriter to capture the response status
// while transparently preserving streaming (Flusher) behavior.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// record publishes a captured request to the KV store.
func record(entry requestLog, statusCode int) {
	kvStore.Record(kv.LogEntry{
		Time:    entry.start.UTC().Format("2006-01-02 15:04:05"),
		IP:      entry.ip,
		Path:    entry.path,
		Model:   entry.model,
		KeyMask: entry.keyMask,
		Status:  statusCode,
		Ms:      time.Since(entry.start).Milliseconds(),
	})
}

// adminHandler renders the management page.
func adminHandler(w http.ResponseWriter, r *http.Request) {
	// Security headers for browser-facing HTML.
	secureHeaders(w.Header())

	if pw := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD")); pw != "" {
		if !constantTimeEqual(r.URL.Query().Get("token"), pw) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"UTF-8\"><title>需要授权</title></head><body><h1>401 需要密码</h1><p>请在 URL 后追加 <code>?token=你的密码</code></p></body></html>"))
			return
		}
	}

	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	st, logs, _ := kvStore.Stats()

	byIP := sortedCounts(st.ByIP)
	byKey := sortedCounts(st.ByKey)

	sort.Slice(logs, func(i, j int) bool { return logs[i].Time > logs[j].Time })

	var b strings.Builder
	b.WriteString(adminHead)

	// Address bar + health.
	b.WriteString("<div class=\"addrbar\"><span class=\"dot\" aria-hidden=\"true\"></span>")
	b.WriteString("<span class=\"lbl\">请求地址</span><code>" + htmlEscape(baseURL) + "</code>")
	b.WriteString("<span class=\"status\">● 正常</span></div>\n")

	if !kvStore.Configured() {
		b.WriteString("<div class=\"warn\">⚠ 未配置 Vercel KV（KV_REST_API_URL / KV_REST_API_TOKEN），当前仅在本进程内累计，Serverless 部署时不会跨请求持久化。</div>\n")
	}

	// Metric cards.
	b.WriteString("<div class=\"cards\">")
	fmt.Fprintf(&b, "<div class=\"card\"><div class=\"num\">%d</div><div class=\"lbl\">总请求数</div></div>", st.Total)
	fmt.Fprintf(&b, "<div class=\"card\"><div class=\"num\">%d</div><div class=\"lbl\">独立 IP</div></div>", len(st.ByIP))
	fmt.Fprintf(&b, "<div class=\"card\"><div class=\"num\">%d</div><div class=\"lbl\">使用 Key</div></div>", len(st.ByKey))
	fmt.Fprintf(&b, "<div class=\"card\"><div class=\"num\">%d</div><div class=\"lbl\">日志条数</div></div>", len(logs))
	b.WriteString("</div>\n")

	// Per-IP and per-Key stats side by side.
	b.WriteString("<div class=\"grid2\">")
	b.WriteString("<section class=\"panel\"><h2>按 IP 统计</h2>")
	if len(byIP) == 0 {
		b.WriteString("<p class=\"empty\">暂无数据</p>")
	} else {
		b.WriteString("<table><thead><tr><th>IP</th><th class=\"num\">请求数</th></tr></thead><tbody>")
		for _, c := range byIP {
			fmt.Fprintf(&b, "<tr><td class=\"mono\">%s</td><td class=\"num\">%d</td></tr>", htmlEscape(c.K), c.V)
		}
		b.WriteString("</tbody></table>")
	}
	b.WriteString("</section>\n")

	b.WriteString("<section class=\"panel\"><h2>按 Key 统计</h2>")
	if len(byKey) == 0 {
		b.WriteString("<p class=\"empty\">暂无数据</p>")
	} else {
		b.WriteString("<table><thead><tr><th>Key（掩码）</th><th class=\"num\">请求数</th></tr></thead><tbody>")
		for _, c := range byKey {
			fmt.Fprintf(&b, "<tr><td class=\"mono\">%s</td><td class=\"num\">%d</td></tr>", htmlEscape(c.K), c.V)
		}
		b.WriteString("</tbody></table>")
	}
	b.WriteString("</section>")
	b.WriteString("</div>\n")

	// Recent request logs.
	b.WriteString("<section class=\"panel\"><h2>最近请求日志</h2>")
	if len(logs) == 0 {
		b.WriteString("<p class=\"empty\">暂无请求</p>")
	} else {
		b.WriteString("<div class=\"logs\"><table><thead><tr><th>时间</th><th>IP</th><th>路径</th><th>模型</th><th>Key</th><th>状态</th><th class=\"num\">耗时</th></tr></thead><tbody>")
		for _, e := range logs {
			pill := "other"
			switch {
			case e.Status >= 200 && e.Status < 300:
				pill = "s2"
			case e.Status == 403 || e.Status == 404:
				pill = "e4"
			case e.Status >= 500:
				pill = "e3"
			}
			fmt.Fprintf(&b, "<tr><td class=\"mono\">%s</td><td class=\"mono\">%s</td><td class=\"mono\">%s</td><td class=\"mono\">%s</td><td class=\"mono\">%s</td><td><span class=\"pill %s\">%d</span></td><td class=\"num\">%d ms</td></tr>",
				e.Time, htmlEscape(e.IP), htmlEscape(e.Path), htmlEscape(e.Model), htmlEscape(e.KeyMask), pill, e.Status, e.Ms)
		}
		b.WriteString("</tbody></table></div>")
	}
	b.WriteString("</section>\n")

	b.WriteString("<footer>NVIDIA NIM Relay · Go · Vercel Serverless</footer></div></body></html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

const adminHead = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>中转服务 · 管理面板</title>
<style>
  :root{
    --bg:#0b0e14; --panel:#11151e; --panel-2:#161b27; --line:#1f2634;
    --text:#e6eaf2; --muted:#8b94a7; --accent:#5ea1ff; --accent-ink:#0b1526;
    --good:#5fd6a6; --warn:#ffc66d; --bad:#ff7a7a;
    --radius:12px;
  }
  *{box-sizing:border-box}
  html{scrollbar-color:var(--line) transparent}
  body{margin:0;background:
    radial-gradient(1200px 600px at 80% -10%, rgba(94,161,255,.10), transparent 60%),
    var(--bg);color:var(--text);
    font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
    -webkit-font-smoothing:antialiased}
  a{color:var(--accent)}
  .wrap{max-width:1120px;margin:0 auto;padding:32px 20px 72px}
  /* Header */
  .top{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;flex-wrap:wrap;margin-bottom:6px}
  .brand{display:flex;align-items:center;gap:14px}
  .logo{width:42px;height:42px;border-radius:11px;flex:none;display:grid;place-items:center;
    background:linear-gradient(135deg,#2a6fdb,#6aa8ff);color:#0b1526;font-weight:800;font-size:20px}
  h1{font-size:20px;margin:0;letter-spacing:.2px}
  .sub{color:var(--muted);font-size:13px;margin:3px 0 0}
  .actions{display:flex;gap:10px;align-items:center}
  .refresh{text-decoration:none;font-size:13px;padding:8px 14px;border:1px solid var(--line);border-radius:8px;
    color:var(--text);background:var(--panel);transition:.15s}
  .refresh:hover{border-color:var(--accent);color:var(--accent)}
  /* Address */
  .addrbar{display:flex;align-items:center;gap:10px;background:var(--panel);border:1px solid var(--line);
    border-radius:var(--radius);padding:12px 16px;margin:20px 0;flex-wrap:wrap}
  .addrbar .dot{width:9px;height:9px;border-radius:50%;background:var(--good);box-shadow:0 0 0 4px rgba(95,214,166,.15)}
  .addrbar .lbl{color:var(--muted);font-size:13px}
  .addrbar code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:14px;color:var(--text)}
  .addrbar .status{margin-left:auto;font-size:12px;color:var(--good);font-weight:600;letter-spacing:.4px}
  /* KV warn */
  .warn{display:flex;gap:10px;align-items:flex-start;color:#ffd9a3;background:#221a0d;border:1px solid #4d3a17;
    padding:12px 14px;border-radius:var(--radius);font-size:13px;margin:-8px 0 18px}
  /* Cards */
  .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:14px;margin:22px 0 8px}
  .card{background:linear-gradient(180deg,var(--panel),var(--panel-2));border:1px solid var(--line);
    border-radius:var(--radius);padding:18px 18px 16px;position:relative;overflow:hidden}
  .card::before{content:"";position:absolute;inset:0 auto 0 0;width:3px;background:var(--accent);opacity:.85}
  .card .num{font-size:34px;font-weight:760;letter-spacing:-.5px;line-height:1.05;font-variant-numeric:tabular-nums}
  .card .lbl{color:var(--muted);font-size:13px;margin-top:6px}
  /* Section */
  h2{font-size:15px;font-weight:650;margin:30px 0 12px;letter-spacing:.2px;display:flex;align-items:center;gap:8px}
  h2::before{content:"";width:4px;height:14px;background:var(--accent);border-radius:2px}
  .grid2{display:grid;grid-template-columns:1fr 1fr;gap:16px}
  @media(max-width:820px){.grid2{grid-template-columns:1fr}}
  .panel{background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);overflow:hidden}
  /* Tables */
  table{width:100%;border-collapse:collapse;font-size:13px}
  thead th{text-align:left;color:var(--muted);font-weight:600;padding:11px 16px;border-bottom:1px solid var(--line);
    font-size:12px;letter-spacing:.3px;background:var(--panel-2)}
  tbody td{padding:10px 16px;border-bottom:1px solid var(--line);vertical-align:top}
  tbody tr:last-child td{border-bottom:none}
  tbody tr:hover td{background:rgba(94,161,255,.05)}
  td.mono, td code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}
  td.num{font-variant-numeric:tabular-nums;text-align:right;white-space:nowrap}
  .pill{display:inline-block;min-width:34px;text-align:center;font-size:12px;font-weight:650;border-radius:6px;
    padding:2px 8px;font-variant-numeric:tabular-nums}
  .pill.s2{background:rgba(95,214,166,.14);color:var(--good)}
  .pill.e3,.pill.e4{background:rgba(255,199,109,.14);color:var(--warn)}
  .pill.other{background:rgba(122,147,255,.16);color:#9ab0ff}
  .empty{color:var(--muted);font-size:13px;padding:18px 16px}
  .logs{max-height:520px;overflow:auto}
  footer{margin-top:40px;color:var(--muted);font-size:12px;text-align:center}
  code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
</style>
</head>
<body>
<div class="wrap">
<header class="top">
  <div class="brand">
    <div class="logo" aria-hidden="true">NIM</div>
    <div>
      <h1>中转服务管理</h1>
      <div class="sub">NVIDIA NIM Relay · 运行观测台</div>
    </div>
  </div>
  <div class="actions"><a class="refresh" href="/admin">刷新 ↻</a></div>
</header>
`

func htmlEscape(s string) string {
	return html.EscapeString(s)
}

// constantTimeEqual compares two strings in constant time to avoid leaking
// password length/prefix via timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// secureHeaders sets a minimal set of security headers for browser-facing
// HTML responses.
func secureHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// No scripts on this page; allow inline styles only.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
}

func sortedCounts(m map[string]int64) []struct {
	K string
	V int64
} {
	out := make([]struct {
		K string
		V int64
	}, 0, len(m))
	for k, v := range m {
		out = append(out, struct {
			K string
			V int64
		}{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].V != out[j].V {
			return out[i].V > out[j].V
		}
		return out[i].K < out[j].K
	})
	return out
}