# NVIDIA NIM API 中转服务

Go 语言实现的 HTTP 反向代理中转服务，用于透明转发 NVIDIA NIM API 请求，解决国内直连延迟高的问题。

## 功能特性

- **透明转发** — 所有路径透明转发至 NVIDIA NIM API，不修改请求/响应内容
- **多 Key 均衡调用** — 多个 API Key 以逗号分隔配置，round-robin 轮询均衡分配
- **SSE 流式转发** — 实时转发 `text/event-stream` 流式响应，逐 chunk flush
- **故障自动切换** — 上游返回 401/429 时自动切换下一个 Key 重试
- **OpenAI SDK 兼容** — 用户只需修改 `base_url` 即可使用
- **Vercel Serverless 部署** — 免运维，自动扩缩容

## 快速开始

### 本地运行

```bash
# 设置环境变量
export NVIDIA_API_KEYS="your-key-1,your-key-2,your-key-3"

# 启动服务
make run
# 或
go run main.go
```

服务默认监听 `:8080`，可通过 `PORT` 环境变量修改。

### 使用示例（OpenAI SDK）

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",  # 改为中转地址
    api_key="any"                          # 任意值，中转会替换为真实 Key
)

completion = client.chat.completions.create(
    model="z-ai/glm-5.2",
    messages=[{"role": "user", "content": "Hello"}],
    stream=True
)

for chunk in completion:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

### Vercel 部署

1. Fork 或导入此仓库到你的 GitHub 账号
2. 在 [Vercel](https://vercel.com) 中导入该仓库
3. 配置环境变量：
   - `NVIDIA_API_KEYS` — 多个 Key 以英文逗号分隔
   - `UPSTREAM_URL`（可选）— 默认 `https://integrate.api.nvidia.com`
4. 部署完成后，使用 Vercel 分配的域名作为 `base_url`

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `NVIDIA_API_KEYS` | 是 | — | NVIDIA API Key，多个以英文逗号分隔 |
| `UPSTREAM_URL` | 否 | `https://integrate.api.nvidia.com` | 上游 API 地址 |
| `PORT` | 否 | `8080` | 本地运行端口 |

## 技术栈

- Go 1.21+（仅标准库，零第三方依赖）
- Vercel Serverless Functions

## 项目结构

```
├── main.go                  # 本地运行入口
├── api/index.go             # Vercel Serverless 入口
├── internal/relay/
│   ├── relay.go             # 核心代理逻辑（重试、SSE 流式、错误处理）
│   ├── keys.go              # 多 Key 轮询管理
│   └── keys_test.go         # 单元测试
├── vercel.json              # Vercel 部署配置
├── Makefile                 # 本地开发命令
└── .env.example             # 环境变量示例
```
