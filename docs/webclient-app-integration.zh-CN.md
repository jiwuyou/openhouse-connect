# Webclient App 接入指南

本文面向需要接入 openhouse-connect webclient 后端的外部 app 开发者。

webclient 后端是一个可托管多个网页客户端 app 的平台服务。每个 app 都有独立的 HTTP 命名空间、Bridge 平台身份和持久化存储目录。外部 app 可以通过自己的前端或后端接入它，而不需要复制一套 webclient 后端。

## 架构定位

推荐链路：

```text
App Frontend
  -> App Backend / Same-origin Proxy
  -> openhouse-connect webclient backend
  -> Bridge
  -> Project Agent
```

webclient backend 与 Feishu、Telegram 等平台同级。对 Bridge 来说，每个 webclient app 都是一个独立的平台 adapter 身份。

不要让 app 前端直接连接 Bridge。app 前端应调用自己的后端，或调用 webclient backend 暴露的 app 命名空间 API。

## 核心概念

每个 app 由三个参数定义：

| 字段 | 用途 | 示例 |
| --- | --- | --- |
| `app_id` | HTTP 路由命名空间 | `crm` |
| `platform` | Bridge 注册平台名 | `web-crm` |
| `data_namespace` | 持久化存储隔离命名空间 | `crm` |

要求：

- `app_id`、`platform`、`data_namespace` 在同一个 webclient 服务内必须唯一。
- 三者建议只使用 `[A-Za-z0-9_-]`。
- `data_namespace` 上线后不要随意修改。修改它等同于切换到另一套聊天数据。

## 配置示例

```toml
data_dir = "/root/.cc-connect"

[webclient]
enabled = true
host = "0.0.0.0"
port = 9840
token = "replace-with-a-secret-token"
default_app = "crm"

[[webclient.apps]]
id = "crm"
platform = "web-crm"
data_namespace = "crm"

[[webclient.apps]]
id = "support"
platform = "web-support"
data_namespace = "support"
```

禁用 app：

```toml
[[webclient.apps]]
id = "support"
platform = "web-support"
data_namespace = "support"
enabled = false
```

禁用后 app 不再注册 adapter，新请求会访问不到该 app，但磁盘数据会保留。

## 路由约定

legacy/default app 路由：

```text
/api/v1/...
/api/projects/...
/attachments/{id}
```

多 app 命名空间路由：

```text
/apps/{app_id}/api/v1/...
/apps/{app_id}/api/projects/...
/apps/{app_id}/attachments/{id}
```

`/api/...` 和 `/attachments/...` 总是指向当前 `default_app`。外部 app 建议使用 `/apps/{app_id}/...`，避免 `default_app` 切换导致数据落到别的 app。

## 常用 API

以下示例假设：

- webclient backend 地址：`http://localhost:9840`
- app id：`crm`
- project：`my-project`
- token：`replace-with-a-secret-token`

鉴权可使用 Bearer token：

```http
Authorization: Bearer replace-with-a-secret-token
```

### 创建或列出会话

```http
GET /apps/crm/api/v1/projects/my-project/sessions
```

```http
POST /apps/crm/api/v1/projects/my-project/sessions
Content-Type: application/json

{
  "name": "default"
}
```

### 发送消息

推荐使用 v1 send API：

```http
POST /apps/crm/api/v1/projects/my-project/send
Content-Type: application/json

{
  "session_key": "web-crm:my-project:default",
  "session_id": "default",
  "message": "帮我看一下这个项目的状态"
}
```

说明：

- `session_key` 用于 Bridge/Agent 侧识别会话来源。
- `session_id` 是 webclient 后端的本地会话 ID。
- 如果 app 自己有业务会话 ID，可以映射到 `session_id`，但必须满足安全路径片段要求。

兼容的非 v1 消息路由：

```http
POST /apps/crm/api/projects/my-project/sessions/default/messages
Content-Type: application/json

{
  "content": "帮我看一下这个项目的状态"
}
```

如果没有可用投递路径，该路由会返回错误，不会静默成功。

### 读取消息

```http
GET /apps/crm/api/v1/projects/my-project/sessions/default
```

或：

```http
GET /apps/crm/api/projects/my-project/sessions/default/messages
```

### 订阅事件

```http
GET /apps/crm/api/projects/my-project/sessions/default/events
```

这是 SSE 事件流。消息事件和运行过程事件会从这里推送给前端。

删除或禁用 app 时，该 app 的 SSE 连接会结束。前端应按普通断线处理。

### 附件访问

附件 URL 应使用后端返回的 `url` 字段，不要由前端自己拼接。

app 命名空间附件路由：

```text
/apps/{app_id}/attachments/{id}
```

legacy/default app 附件路由：

```text
/attachments/{id}
```

如果设置了 `webclient.public_url`，后端返回的附件 URL 会使用该公网前缀。

## 图片和文件

发送图片时，使用 v1 send API 的 `images` 字段。每张图片一般使用 base64 data：

```json
{
  "session_key": "web-crm:my-project:default",
  "session_id": "default",
  "message": "请分析这张图片",
  "images": [
    {
      "mime_type": "image/png",
      "file_name": "screenshot.png",
      "data": "..."
    }
  ]
}
```

Agent 返回图片或文件时，webclient backend 会持久化附件，并在消息里返回可访问 URL。

## 存储隔离

webclient 后端统一管理存储。

legacy 单 app：

```text
{data_dir}/webclient/
```

multi-app：

```text
{data_dir}/webclient/apps/{data_namespace}/
```

每个 app 目录内包含：

```text
messages/
sessions/
attachments/
outbox/
run_events/
settings.json
```

外部 app 不应该直接读写这些目录。它们是 webclient backend 的内部持久化格式。

## 推荐接入方式

### 方式一：同源代理

如果你的 app 有自己的后端，推荐由 app 后端把某个前缀代理到 webclient backend：

```text
https://crm.example.com/openhouse/*
  -> http://127.0.0.1:9840/apps/crm/*
```

优点：

- 前端保持同源，不需要处理跨源细节。
- token 可以留在后端，不暴露给浏览器。
- app 后端可以叠加自己的登录、权限、审计和业务逻辑。

### 方式二：跨源直接调用

也可以让浏览器直接访问 webclient backend，但需要处理：

- CORS
- token 暴露风险
- 浏览器端鉴权存储
- HTTPS 与 cookie/header 策略

除非是内网可信环境，否则不推荐把 webclient token 直接交给前端。

## 多 app 设计建议

多个 app 不需要多套代码，也不需要启动多个 9840。

推荐：

```toml
[[webclient.apps]]
id = "crm"
platform = "web-crm"
data_namespace = "crm"

[[webclient.apps]]
id = "support"
platform = "web-support"
data_namespace = "support"

[[webclient.apps]]
id = "ops"
platform = "web-ops"
data_namespace = "ops"
```

外部 app 通过不同 `app_id` 接入：

```text
/apps/crm/...
/apps/support/...
/apps/ops/...
```

如果某个外部后端只想暴露部分 app，可以只代理对应前缀。

## 热更新

当前支持通过 reload 热更新 webclient apps。

支持热更新：

- 新增 `[[webclient.apps]]`
- 删除 app
- 设置 `enabled = false`
- 切换 `default_app`
- 修改 app 的 `platform`，对应 adapter 会重启并重新注册

不支持热更新，需要重启：

- `[webclient].host`
- `[webclient].port`
- `[webclient].data_dir`
- app 的 `data_namespace`
- legacy 单 app 与 multi-app 模式互相切换
- 整个 `[webclient].enabled` 从 false 到 true，或从 true 到 false

触发 reload 的方式：

- 管理端系统设置中的 reload
- 管理 API 的 reload 接口
- 支持 `/config reload` 的平台会话命令

注意：

- 管理端 reload 可能会按项目重复触发，webclient hot reload 是幂等的。
- 删除或禁用 app 会断开该 app 的 SSE 事件流。
- 磁盘数据不会因为删除/禁用 app 自动删除。

## 安全建议

- 为 `webclient.token` 设置强随机值。
- 不要把 token 写进前端源码。
- 如果 app 有自己的登录系统，应由 app 后端代理 webclient API。
- 不要让用户可控输入直接成为 `app_id`、`platform`、`data_namespace`。
- 不要直接暴露 `{data_dir}/webclient/apps/*` 目录。

## 接入检查清单

- [ ] 已为 app 分配唯一 `app_id`
- [ ] 已为 app 分配唯一 `platform`
- [ ] 已为 app 分配稳定的 `data_namespace`
- [ ] 前端使用 `/apps/{app_id}/...`，没有依赖 legacy `/api/...`
- [ ] 附件 URL 使用后端返回值
- [ ] app 后端负责 token 和用户权限
- [ ] reload 后验证 app 路由可访问
- [ ] 确认禁用/删除 app 不会删除磁盘数据

