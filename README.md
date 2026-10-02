# NVIDIA NIM API 中转服务

Go 语言实现的 HTTP 反向代理中转服务，用于透明转发 NVIDIA NIM API 请求，解决国内直连延迟高的问题。

## 功能特性

- **纯透明转发** — 所有请求透明转发至 NVIDIA NIM API，不修改任何内容
- **使用原平台 Key** — 客户端直接使用自己的 NVIDIA API Key 调用中转域名，中转原样透传
- **SSE 流式转发** — 实时转发 `text/event-stream` 流式响应，逐 chunk flush
- **OpenAI SDK 兼容** — 用户只需修改 `base_url` 即可使用
- **Responses API 兼容** — 支持 OpenAI `/v1/responses`，自动转换为上游 Chat Completions 协议
- **管理面板** — 内置 `/admin` 页面，可查看请求地址、IP、模型、Key 掩码与请求日志
- **Vercel Serverless 部署** — 免运维，自动扩缩容

---

## 目录

1. [部署](#部署)
2. [使用方法](#使用方法)
3. [管理面板](#管理面板)
4. [健康检查](#健康检查)
5. [环境变量](#环境变量)
6. [安全说明](#安全说明)
7. [项目结构](#项目结构)

---

## 部署

1. Fork 或导入此仓库到你的 GitHub 账号
2. 在 [Vercel](https://vercel.com) 中导入该仓库
3. 按需配置环境变量（见下文「环境变量」一节）
4. 部署完成后，将 Vercel 分配的域名作为 `base_url` 使用

你也可能希望绑定自定义域名（如 `mybiog.us.ci`），在 Vercel 项目的 **Settings → Domains** 中添加即可。

---

## 使用方法

中转服务使用透明的反向代理：**调用方携带自己的 NVIDIA API Key**，中转服务原样转发，不做任何鉴权或密钥管理。所有可用端点见下表：

| 端点 | 说明 |
|------|------|
| `GET /` | 首页（显示「您已成功部署本项目」） |
| `GET /health` | 健康检查 |
| `GET /admin` | 管理面板 |
| `/v1/chat/completions` | Chat Completions 接口（透传） |
| `/v1/responses` | Responses 接口（自动转换为上游协议） |

### 1. 修改 `base_url` 即可用

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://your-vercel-domain.vercel.app/v1",  # 中转地址
    api_key="nvapi-your-nvidia-key-here"                  # 你的 NVIDIA API Key
)

completion = client.chat.completions.create(
    model="z-ai/glm-5.2",                      # 或 kimi-k3 等 NIM 支持模型
    messages=[{"role": "user", "content": "Hello"}],
    stream=True
)

for chunk in completion:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

### 2. 直接 HTTP 调用（curl）

```bash
curl https://your-vercel-domain.vercel.app/v1/chat/completions \
  -H "Authorization: Bearer nvapi-your-nvidia-key-here" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "moonshotai/kimi-k3",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

### 3. 流式响应

当请求中的 `stream: true` 时，上游返回 `text/event-stream`，中转服务会实时逐 chunk 转发，不会等待全部结果，首包延迟低。

### 4. 视觉问答 / 多模态图片输入

NIM 支持图片输入，只需在 `messages` 中传入 `image_url`：

```python
client = OpenAI(base_url="https://your-vercel-domain.vercel.app/v1", api_key="nvapi-...")
resp = client.chat.completions.create(
    model="moonshotai/kimi-k3",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "What is in this image?"},
            {"type": "image_url", "image_url": {"url": "https://example.com/a.jpg"}},
        ],
    }],
    max_tokens=1024,
)
print(resp.choices[0].message.content)
```

### 5. 本地运行调试

```bash
# 启动服务（无需配置 API Key，客户端自己携带）
go run main.go
```

服务默认监听 `:8080`，可通过 `PORT` 环境变量修改。本地即可直接访问 `http://localhost:8080/v1`。

---

## 管理面板

访问 `/admin` 即可打开管理面板，展示：

- **请求地址** — 当前中转服务的访问地址
- **总请求数 / 独立 IP / 使用 Key 数 / 日志条数** 概览卡片
- **按 IP 统计** — 各调用来源 IP 的请求数
- **按 Key 统计** — 各调用方 API Key 的请求数（仅显示掩码，如 `sk-…xxxx [a1b2c3d4]`，不泄露完整 Key）
- **最近请求日志** — 时间、IP、路径、模型、Key 掩码、HTTP 状态码、耗时

设置管理密码后，访问 `/admin?token=你的密码` 才能查看（见下文环境变量）。数据记录：

- 配置了 **Vercel KV** 时，统计数据跨请求持久化；
- 未配置 KV 时，仅在同一进程内累计，Serverless 部署下不会跨实例持久化（页面会显示提示）。

---

## 健康检查

```bash
curl https://your-vercel-domain.vercel.app/health
```

返回：

```json
{"status":"ok","service":"nim-relay","upstream":"https://integrate.api.nvidia.com"}
```

---

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `UPSTREAM_URL` | 否 | `https://integrate.api.nvidia.com` | 上游 API 地址 |
| `PORT` | 否 | `8080` | 本地运行端口 |
| `ADMIN_PASSWORD` | 否 | 空 | 访问 `/admin` 的密码，追加到 URL 的 `?token=密码` |
| `KV_REST_API_URL` | 否 | 空 | 管理面板统计持久化（Vercel KV / Upstash Redis）REST 地址 |
| `KV_REST_API_TOKEN` | 否 | 空 | 管理面板统计持久化对应的 REST Token |

示例见 [.env.example](.env.example)。

> 安全提示：`.env` 已被 `.gitignore` 忽略，请勿把 `ADMIN_PASSWORD`、`KV_REST_API_TOKEN` 等敏感信息写入代码或提交到仓库。

---

## 安全说明

- 调用方只提交自己的 API Key，中转不存库，仅以 SHA-256 哈希 + 尾号形式在管理面板展示
- 请求体上限 10 MiB，防止内存耗尽
- 密码采用常数时间比较，并设置 `nosniff`、`X-Frame-Options`、`Referrer-Policy`、CSP 等安全响应头
- 服务器配置了读超时、空闲超时与头部大小限制

---

## 项目结构

```
├── main.go                  # 本地运行入口
├── api/
│   ├── index.go             # Vercel Serverless 入口（含核心代理逻辑）
│   ├── admin.go             # 管理面板页面与统计逻辑
│   └── responses.go         # OpenAI Responses API → Chat Completions 转换
├── internal/kv/kv.go        # KV 持久化（Upstash Redis / 内存降级）
├── vercel.json              # Vercel 部署配置
├── Makefile                 # 本地开发命令
└── .env.example             # 环境变量示例
```