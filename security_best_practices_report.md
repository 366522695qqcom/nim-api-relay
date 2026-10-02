# 安全审查报告 - nim-relay

审查日期：2026-10-02
技术栈：Go（标准库 net/http，Serverless 于 Vercel / 本地 main.go）
审查依据：[security-best-practices / golang-general-backend-security.md]

## 执行摘要
中转服务整体清晰、无帮助 user 可控输入执行 shell/SQL/模板的危险 sink，也无 open redirects、pprof 暴露等常见故障。本次发现 3 项高危（DoS 与计时侧信道、XSS 转义不完整）与若干中低危（缺安全头、服务器超时未配置、转发头信任依赖边缘）。修复优先级建议：先补 body 上限与收口密码比较，再补安全头与服务器超时。

## Critical
无。

## High
### H-1 / GO-HTTP-002：请求体无大小上限（DoS / 内存耗尽）
- Severity: High
- Location: `api/index.go` Handler 转发路径 `io.ReadAll(r.Body)`（约 L230）；`api/admin.go` extractModel → `readBody(r)`（readAllCloser 无限制）；`api/responses.go` readBody。
- Evidence: `b, err := io.ReadAll(r.Body)` 无 `http.MaxBytesReader` 包裹。
- Impact: 攻击者可发送超大 body 拖垮 Serverless 内存 / 本地服务进程。
- Fix: 在 Handler 入口用 `http.MaxBytesReader(w, r.Body, maxBodyBytes)` 包裹后赋值回 `r.Body`。

### H-2 / GO-AUTH-001：管理密码明文 `!=` 比较（计时侧信道）
- Severity: High
- Location: `api/admin.go` adminHandler `r.URL.Query().Get("token") != pw`（约 L143）。
- Evidence: 普通字符串 `!=` 比较逐字节短路返回，泄露长度/前缀信息。
- Impact: 可被计时攻击逐步探测 `ADMIN_PASSWORD`。
- Fix: 改用 `crypto/subtle.ConstantTimeCompare([]byte(token), []byte(pw))`，并按 `sub.HexDecode` 或直接字节比较；长度不同时返回 0。

### H-3 / GO-XSS-001：HTML 转义不完整（自定义转义漏掉引号）
- Severity: High
- Location: `api/admin.go` htmlEscape 仅转义 `& < >`（约 L256-258），手持实现。
- Evidence: 漏转 `"` 与 `'`；动态值（IP、路径、模型、Key、地址）当前仅进入元素文本节点，暂未直接造成属性注入，但属易错点，且规范要求用标准库转义。
- Fix: 使用标准库 `html.EscapeString`（同时转义 `& < > " '`）替代自定义函数，动态数据一律经其输出。

## Medium
### M-1 / GO-HTTP-004：HTML 响应缺少安全响应头
- Severity: Medium
- Location: `api/admin.go` 与 `api/index.go` 的 HTML 输出（`w.Header().Set(...)`）。
- Evidence: 仅设置 `Content-Type` 与 `Cache-Control`。
- Fix: 为 HTML 响应统一设置 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、(尽量) `Content-Security-Policy`（本项目无脚本，可 `default-src 'none'`，允许内联样式需放宽 style）。

### M-2 / GO-HTTP-001：HTTP 服务器未配置超时与 MaxHeaderBytes
- Severity: Medium（本项主要影响本地 main.go；Vercel 运行时由平台管理）
- Location: `main.go` 使用 `http.ListenAndServe`（无显式 `http.Server` 配置）。
- Evidence: 默认 timeouts 为零（无限制），无 `MaxHeaderBytes`。
- Impact: 慢速连接 / 超大请求头可占用资源。
- Fix: 构造 `http.Server{Addr, Handler, ReadHeaderTimeout, IdleTimeout, MaxHeaderBytes}`。

## Low / Info
### I-1 / GO-HTTP-003：信任 X-Forwarded-For / X-Forwarded-Proto
- Location: `api/admin.go` clientIP、scheme 判断。
- 说明: 部署在 Vercel 边缘之后，这些头由受信平台写入，可接受。建议本地/自托管时在边缘拒绝伪造转发头；Admin 密码走 `?token=`，绝不写入日志（当前未记录）。

### I-2：/admin 密码经 URL 查询参数传递
- 说明: token 会出现在缓存、日志、referrer 与浏览器历史。仅建议性：如可接受，保留现状；更稳妥是可改为 `Authorization` 头 + 受保护端点，但不作强制。

## 已修复项（随本次提交一并落地）
- 见上文 H-1/H-2/H-3/M-1/M-2 对应 Fix，全部实现并附带常量时间比较、MaxBytesReader、stdlib 转义与安全头。