package api

import "strings"

// indexHTML returns the landing page HTML. {{BASE_URL}} is replaced with the
// public base URL of the relay service so the snippets are copy-paste ready.
func indexHTML(baseURL string) string {
	const tpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<title>NVIDIA NIM API Relay · 中转服务</title>
<meta name="description" content="透明转发 NVIDIA NIM API 的中转服务，解决国内直连延迟高的问题。" />
<style>
  :root {
    --bg: #0b0f17;
    --bg-soft: #121826;
    --card: #161d2c;
    --border: #232c40;
    --text: #e6edf6;
    --muted: #8a96ad;
    --accent: #76b900;
    --accent-soft: rgba(118, 185, 0, 0.12);
    --code-bg: #0a0e16;
    --danger: #ff6b6b;
  }
  * { box-sizing: border-box; }
  html, body { margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
      "Hiragino Sans GB", "Microsoft YaHei", Roboto, Helvetica, Arial, sans-serif;
    background: radial-gradient(1200px 600px at 80% -10%, rgba(118,185,0,0.08), transparent 60%),
                radial-gradient(900px 500px at -10% 10%, rgba(0, 122, 255, 0.08), transparent 55%),
                var(--bg);
    color: var(--text);
    line-height: 1.6;
    min-height: 100vh;
  }
  a { color: var(--accent); text-decoration: none; }
  a:hover { text-decoration: underline; }
  .wrap { max-width: 920px; margin: 0 auto; padding: 0 20px; }

  header.hero { padding: 72px 0 40px; text-align: center; }
  .badge {
    display: inline-flex; align-items: center; gap: 8px;
    background: var(--accent-soft); color: var(--accent);
    border: 1px solid rgba(118,185,0,0.3);
    padding: 6px 14px; border-radius: 999px; font-size: 13px; font-weight: 600;
    margin-bottom: 22px;
  }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--accent);
    box-shadow: 0 0 0 0 rgba(118,185,0,0.7); animation: pulse 2s infinite; }
  @keyframes pulse {
    0% { box-shadow: 0 0 0 0 rgba(118,185,0,0.6); }
    70% { box-shadow: 0 0 0 8px rgba(118,185,0,0); }
    100% { box-shadow: 0 0 0 0 rgba(118,185,0,0); }
  }
  h1 { font-size: 40px; margin: 0 0 14px; font-weight: 700; letter-spacing: -0.5px;
    background: linear-gradient(135deg, #fff 0%, #b9c6da 100%);
    -webkit-background-clip: text; background-clip: text; -webkit-text-fill-color: transparent;
  }
  .subtitle { color: var(--muted); font-size: 18px; margin: 0 auto; max-width: 620px; }
  .upstream-tag { display: inline-block; margin-top: 16px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 13px; color: var(--muted); background: var(--bg-soft); border: 1px solid var(--border);
    padding: 6px 12px; border-radius: 8px; }

  section { padding: 28px 0; }
  h2 { font-size: 22px; margin: 0 0 16px; font-weight: 600; display: flex; align-items: center; gap: 10px; }
  h2 .num { color: var(--accent); font-size: 14px; font-family: ui-monospace, monospace;
    background: var(--accent-soft); width: 26px; height: 26px; border-radius: 7px;
    display: inline-flex; align-items: center; justify-content: center; }

  .card { background: var(--card); border: 1px solid var(--border); border-radius: 14px;
    padding: 22px 24px; }
  .grid { display: grid; gap: 14px; }
  .grid.cols-2 { grid-template-columns: 1fr 1fr; }
  @media (max-width: 720px) { .grid.cols-2 { grid-template-columns: 1fr; } }

  .feature { display: flex; gap: 14px; align-items: flex-start; }
  .feature .ic { color: var(--accent); font-size: 18px; margin-top: 2px; }
  .feature h3 { margin: 0 0 4px; font-size: 15px; font-weight: 600; }
  .feature p { margin: 0; color: var(--muted); font-size: 14px; }

  .code {
    background: var(--code-bg); border: 1px solid var(--border); border-radius: 10px;
    overflow: hidden; position: relative;
  }
  .code .tabbar { display: flex; border-bottom: 1px solid var(--border); background: var(--bg-soft); }
  .code .tab { padding: 10px 16px; font-size: 13px; color: var(--muted); cursor: pointer;
    border-right: 1px solid var(--border); user-select: none; transition: all .15s; }
  .code .tab.active { color: var(--text); background: var(--code-bg); }
  .code pre { margin: 0; padding: 16px 18px; overflow-x: auto; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 13.5px; line-height: 1.65; }
  .code pre[data-lang] { display: none; }
  .code pre.active { display: block; }
  .c-kw { color: #ff7b72; } .c-str { color: #a5d6ff; } .c-cmt { color: #6e7681; }
  .c-fn { color: #d2a8ff; } .c-var { color: #79c0ff; } .c-num { color: #ffa657; }

  .endpoint { display: flex; align-items: center; gap: 12px; padding: 10px 0;
    border-bottom: 1px dashed var(--border); }
  .endpoint:last-child { border-bottom: none; }
  .method { font-family: ui-monospace, monospace; font-size: 12px; font-weight: 700;
    padding: 3px 8px; border-radius: 5px; min-width: 56px; text-align: center; }
  .method.get { color: #76b900; background: rgba(118,185,0,0.1); }
  .method.post { color: #79c0ff; background: rgba(121,192,255,0.1); }
  .path { font-family: ui-monospace, monospace; font-size: 14px; color: var(--text); }
  .desc { color: var(--muted); font-size: 13px; margin-left: auto; }

  .note { background: rgba(0, 122, 255, 0.08); border: 1px solid rgba(0,122,255,0.25);
    color: #b9d6ff; padding: 14px 18px; border-radius: 10px; font-size: 14px; }
  .note strong { color: #dceaff; }
  .warn { background: rgba(255, 200, 0, 0.08); border: 1px solid rgba(255,200,0,0.25);
    color: #ffe9a8; padding: 14px 18px; border-radius: 10px; font-size: 14px; }

  footer { text-align: center; color: var(--muted); font-size: 13px; padding: 40px 0 30px;
    border-top: 1px solid var(--border); margin-top: 30px; }
  footer code { background: var(--bg-soft); padding: 2px 7px; border-radius: 5px;
    font-family: ui-monospace, monospace; color: var(--accent); }
  .copy-btn { position: absolute; top: 8px; right: 8px; background: var(--bg-soft);
    border: 1px solid var(--border); color: var(--muted); padding: 4px 10px;
    border-radius: 6px; font-size: 12px; cursor: pointer; transition: all .15s; }
  .copy-btn:hover { color: var(--text); border-color: var(--accent); }
</style>
</head>
<body>
  <header class="hero">
    <div class="wrap">
      <span class="badge"><span class="dot"></span> 服务运行中</span>
      <h1>NVIDIA NIM API Relay</h1>
      <p class="subtitle">透明转发 NVIDIA NIM API，使用你自己的 API Key 调用中转域名，解决国内直连延迟高的问题。</p>
      <div class="upstream-tag">→ integrate.api.nvidia.com</div>
    </div>
  </header>

  <div class="wrap">
    <section>
      <h2><span class="num">1</span> 快速开始</h2>
      <div class="card grid" style="gap:18px">
        <p style="margin:0;color:var(--muted);font-size:15px">
          只需把 OpenAI SDK 的 <code style="background:var(--bg-soft);padding:2px 6px;border-radius:5px;color:var(--accent)">base_url</code>
          改为中转地址，并继续使用你自己的 NVIDIA API Key，中转会原样透传请求与响应（含 SSE 流式）。
        </p>
        <div class="code">
          <div class="tabbar">
            <div class="tab active" data-tab="python">Python · OpenAI SDK</div>
            <div class="tab" data-tab="curl">cURL</div>
          </div>
          <button class="copy-btn" onclick="copyCode(this)">复制</button>
          <pre class="active" data-lang="python"><span class="c-cmt"># 你的中转地址</span>
<span class="c-kw">from</span> openai <span class="c-kw">import</span> OpenAI

client = OpenAI(
    base_url=<span class="c-str">"{{BASE_URL}}/v1"</span>,
    api_key=<span class="c-str">"nvapi-your-nvidia-key"</span>,   <span class="c-cmt"># 你的 NVIDIA API Key</span>
)

completion = client.chat.completions.create(
    model=<span class="c-str">"z-ai/glm-5.2"</span>,
    messages=[{<span class="c-str">"role"</span>: <span class="c-str">"user"</span>, <span class="c-str">"content"</span>: <span class="c-str">"你好"</span>}],
    stream=<span class="c-kw">True</span>,
)

<span class="c-kw">for</span> chunk <span class="c-kw">in</span> completion:
    <span class="c-kw">if</span> chunk.choices[<span class="c-num">0</span>].delta.content:
        print(chunk.choices[<span class="c-num">0</span>].delta.content, end=<span class="c-str">""</span>)</pre>
          <pre data-lang="curl">curl {{BASE_URL}}/v1/chat/completions \
  -H <span class="c-str">"Authorization: Bearer nvapi-your-nvidia-key"</span> \
  -H <span class="c-str">"Content-Type: application/json"</span> \
  -d <span class="c-str">'{
    "model": "z-ai/glm-5.2",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'</span></pre>
        </div>
      </div>
    </section>

    <section>
      <h2><span class="num">2</span> 特性</h2>
      <div class="card grid cols-2">
        <div class="feature">
          <div class="ic">◆</div>
          <div><h3>纯透明转发</h3><p>不修改请求与响应内容，Authorization 原样透传给 NVIDIA。</p></div>
        </div>
        <div class="feature">
          <div class="ic">◆</div>
          <div><h3>SSE 流式</h3><p>逐 chunk 实时 flush，支持 <code style="color:var(--accent)">stream: true</code> 流式响应。</p></div>
        </div>
        <div class="feature">
          <div class="ic">◆</div>
          <div><h3>OpenAI 兼容</h3><p>任何 OpenAI 兼容客户端只改 <code style="color:var(--accent)">base_url</code> 即可使用。</p></div>
        </div>
        <div class="feature">
          <div class="ic">◆</div>
          <div><h3>Serverless 部署</h3><p>基于 Vercel 边缘网络，免运维、自动扩缩容。</p></div>
        </div>
      </div>
    </section>

    <section>
      <h2><span class="num">3</span> 端点</h2>
      <div class="card">
        <div class="endpoint">
          <span class="method post">POST</span>
          <span class="path">/v1/chat/completions</span>
          <span class="desc">对话补全（支持流式）</span>
        </div>
        <div class="endpoint">
          <span class="method post">POST</span>
          <span class="path">/v1/embeddings</span>
          <span class="desc">文本向量化</span>
        </div>
        <div class="endpoint">
          <span class="method get">GET</span>
          <span class="path">/v1/models</span>
          <span class="desc">列出可用模型</span>
        </div>
        <div class="endpoint">
          <span class="method get">GET</span>
          <span class="path">/health</span>
          <span class="desc">服务健康检查</span>
        </div>
      </div>
    </section>

    <section>
      <h2><span class="num">4</span> 提示</h2>
      <div class="grid" style="gap:12px">
        <div class="warn">
          <strong>关于延迟：</strong> 该中转已部署在 Vercel 边缘网络。若仍感觉较慢，可能由以下原因导致：
          Cloudflare 代理叠加了一层延迟、Vercel 函数冷启动、或 NVIDIA 上游本身响应慢。
          建议检查 Cloudflare DNS 记录的「代理状态」（橙色云 → 灰色云可减少一跳）。
        </div>
        <div class="note">
          <strong>关于 Key：</strong> 本服务不存储或管理任何 API Key。请求头中的
          <code style="color:var(--accent)">Authorization</code> 由客户端提供并原样转发给 NVIDIA，请妥善保管你的 Key。
        </div>
      </div>
    </section>
  </div>

  <footer>
    <div class="wrap">
      NVIDIA NIM API Relay · 部署于 Vercel Serverless<br/>
      健康检查 <code>/health</code> · 中转地址 <code>{{BASE_URL}}</code>
    </div>
  </footer>

<script>
  document.querySelectorAll('.code .tab').forEach(function(tab){
    tab.addEventListener('click', function(){
      var code = tab.closest('.code');
      code.querySelectorAll('.tab').forEach(function(t){ t.classList.remove('active'); });
      code.querySelectorAll('pre').forEach(function(p){ p.classList.remove('active'); });
      tab.classList.add('active');
      code.querySelector('pre[data-lang="' + tab.dataset.tab + '"]').classList.add('active');
    });
  });
  function copyCode(btn){
    var active = btn.parentElement.querySelector('pre.active');
    var text = active ? active.innerText : '';
    navigator.clipboard.writeText(text).then(function(){
      btn.innerText = '已复制';
      setTimeout(function(){ btn.innerText = '复制'; }, 1500);
    });
  }
</script>
</body>
</html>`
	return strings.ReplaceAll(tpl, "{{BASE_URL}}", baseURL)
}
