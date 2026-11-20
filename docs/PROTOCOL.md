# Meoo 接口抓包记录

抓包时间：2026-11-20。目标：`https://meoo.com/chat/jayz1kqo4vf5`（一个已登录的会话页）。
方法：浏览器调试抓包 + 前端 bundle（`g.alicdn.com/meoo/meoo-web/0.0.113/assets/bootstrapApp.js`，3.2 MB）静态分析 + 直连接口探测。

结论先行：**有两条独立通道，能力不同**。

| 通道 | 鉴权 | 能力 | 稳定性 |
|---|---|---|---|
| 官方 compatible-mode | 项目 AK `meoo_project_…` | 纯模型推理（标准 OpenAI） | 高，本项目的实现主线 |
| Web agent 协议 | 网页 Cookie | 建站、sandbox、文件、预览 | 低，事件要自己拼 |

---

## 一、官方兼容网关

```
https://api.meoo.host/meoo-ai/compatible-mode/v1
```

无 AK 时的真实响应（证明端点存在）：

```bash
curl https://api.meoo.host/meoo-ai/compatible-mode/v1/models -H 'Authorization: Bearer test'
# HTTP 401
{"error":{"type":"invalid_request_error","message":"项目服务 AK 格式非法","code":"SERVICE_AK_FORMAT_INVALID"}}
```

标准 OpenAI 形态，支持 `/models`、`/chat/completions`（流式走 `text/event-stream`）。
注意错误码 `SERVICE_AK_FORMAT_INVALID` 是**格式校验先于鉴权**，所以拿到这个错说明 AK 前缀不对。

---

## 二、设备授权（拿个人 AK）

三步，全程 HTTP，不需要 Cookie、不需要浏览器自动化。

### 1. 创建会话

```http
POST https://meoo.com/api/v1/cli/auth/session
Content-Type: application/json
X-Meoo-Client: cli

{}
```

```json
{"success":true,"data":{"sessionId":"58e336b3845840e08db70b996dd6be43"}}
```

### 2. 用户在浏览器确认

```
https://meoo.com/cli/login?session_id=<sessionId>
```

注意路径是 **`/cli/login`**，不是 `/cli-login`（写错会 404 或跳首页）。会话 15 分钟有效。

### 3. 轮询结果

```http
GET https://meoo.com/api/v1/cli/auth/session/<sessionId>
X-Meoo-Client: cli
```

```json
// 未确认
{"success":true,"data":{"status":"pending"}}

// 已确认（只返回一次 apiKey）
{"success":true,"data":{"status":"approved","apiKey":"meoo_ak_…","userId":"3821476…","username":"…"}}
```

`status` 取值：`pending` / `approved` / `denied` / `expired`。

**坑**：`approved` 里的 `apiKey` 只读得到一次，重复轮询会拿空。服务端必须缓存，并且同一会话的轮询要串行（并发轮询会丢凭证）。

---

## 三、项目开通（个人 AK → 项目 AK）

所有请求带头：`Authorization: Bearer meoo_ak_…` + `X-Meoo-Client: cli`。

### 1. 身份校验

```http
GET https://meoo.com/open/v1/cli-compat/account/identity
```

```json
{"data":{"user_id":"3821…9872","username":"…","name":"…"}}
```

### 2. 积分

```http
GET https://meoo.com/open/v1/cli-compat/account/points
```

```json
{"data":{"account":{"availableBalance":6986}}}
```

### 3. 建项目

```http
POST https://meoo.com/open/v1/cli-compat/projects

{"name":"meoo2api-abc","type":"create-desktop"}
```

```json
{"data":{"url_id":"jayz1kqo4vf5"}}
```

### 4. 开通云服务

```http
POST https://meoo.com/open/v1/cli-compat/projects/{url_id}/cloud

{"description":"meoo2api-abc cloud"}
```

`409` 表示已开通，视为成功。额度满会返回业务错误 `QUOTA_EXCEEDED`。

### 5. 读项目 AK

```http
GET https://meoo.com/open/v1/cli-compat/projects/{url_id}/cloud/environment
```

```json
{"data":{"meooProjectApiKey":"meoo_project_…","enabled":true}}
```

开通后可能异步生效，需要有限重试（本项目 6 次 × 2 秒）。

**幂等**：建项目成功后立刻把 `url_id` 落盘。重试只续做「开通云服务 → 读 AK」，不会重复建项目。

---

## 四、Web agent 协议（备查，本项目未采用）

页面加载后会拉一堆接口，核心是这几条：

| 用途 | 接口 |
|---|---|
| 模型列表 | `GET /api/v1/agent/models` |
| 当前用户 | `GET /api/v1/user/me` |
| 项目初始化 | `GET /api/v1/project/{url_id}/init` |
| 项目消息历史 | `GET /api/v1/project/{url_id}/message-history?page_size=50` |
| 任务消息 | `GET /api/v1/agent/chat/messages?change_id=0&task_id=…&page=1&page_size=1000` |
| 发消息 | `POST /api/agent/start` |
| 事件流（SSE） | `GET /api/agent/query?projectId=…` |
| 中断 | `POST /api/agent/stop` |
| 内容安全预检 | `POST /api/v1/content-security/check-text` |

### 发消息的真实请求体

```json
{
  "question": "1+1=?",
  "chatType": "BOLT_CLAUDE",
  "model": "qwen3.8-flash",
  "role": "user",
  "taskId": "bdfa4c26-d1d0-48c8-a529-6c60812e7597",
  "clientMessageId": "tmwoz11p0",
  "message": "1+1=?",
  "mode": "agent",
  "port": 3001,
  "projectId": "jayz1kqo4vf5",
  "skills": [],
  "mcpServers": {"baseUrl": "", "mcpServers": {}},
  "message_owner": {"user_id": "…", "username": "…", "avatar": ""},
  "createMode": "create-desktop",
  "extraParams": {
    "speedTier": "standard",
    "args": {"inputs": {"create_mode": "create-desktop"}, "skillsSnapshot": [], "extraParams": {"speedTier": "standard"}},
    "llmInfo": {"createMode": "create-desktop", "llmMode": "qwen3.8-flash"}
  }
}
```

响应只是受理回执：

```json
{"success":true,"data":{"taskId":"…","projectId":"…","generationId":"gen_1795178668621_3bh5cy","status":"accepted","replayed":false}}
```

### SSE 事件序列（实测一次完整对话）

```
data: {"type":"connected","timestamp":…,"pid":8419}
data: {"type":"agent_state","data":{"status":"idle",…}}
data: {"type":"state_change","data":{"model":"qwen3.8-flash",…}}
data: {"type":"ui_event","data":{"type":"ui:agent_chat_state","data":{"waitingHint":"正在处理你的请求…"}}}
data: {"type":"ui_event","data":{"type":"ui:register_message_id","data":{"serverMessageId":"1d5ee6e4-…","userMessage":{…}}}}
data: {"type":"ui_event","data":{"type":"ui:agent_message_update","data":{"messageId":"1d5ee6e4-…","delta":"1"}}}
data: {"type":"ui_event","data":{"type":"ui:agent_message_update","data":{"delta":" +"}}}
data: {"type":"task_id","text":"bdfa4c26-…"}
data: {"type":"ui_event","data":{"type":"ui:agent_finish_reason","data":{"finish_reason":"stop"}}}
data: {"type":"ui_event","data":{"type":"ui:agent_token_usage","data":{"promptTokens":59,"completionTokens":14,"cacheTokens":29886}}}
data: {"type":"state_change","data":{"completionMetadata":{"usageInfo":{"inputTokens":29945,"outputTokens":14}}}}
data: {"type":"task_complete","text":"1 + 1 = 2。\n\n有其他问题随时问我。"}
```

正文靠 `ui:agent_message_update.delta` 增量累加；`task_complete.text` 是最终全文；`task_id` 事件给出 taskId。

### 历史消息结构

`GET /api/v1/agent/chat/messages` 返回里，**user 的 content 是 JSON 字符串**，assistant 的是纯 markdown：

```json
{"role":"user","content":"[{\"type\":\"text\",\"text\":\"hi\"}]"}
{"role":"assistant","content":"你好！我是 Meoo…","metadata":{"usageInfo":{"inputTokens":29143,"outputTokens":145,"contextWindowTokens":200000},"creditConsumed":4.9396,"actualModel":"qwen3.8-flash"}}
```

### 上游模型目录（`GET /api/v1/agent/models`）

`qwen3.8-flash`、`qwen3.8-max`、`qwen4-max`、`qwen3.7-max`、`qwen3.7-plus`、
`deepseek-v4.1-flash-1017`、`deepseek-v4-pro-0813`、`deepseek-v4-flash-0731`、
`kimi-k3`、`kimi-k3.1-pro`、`auto`。

每条带 `context_window`（多为 200000）、`max_token`（64000 / 16384）、`multimodal`、`reasoningControl`。

---

## 五、两条通道怎么选

- 只要「把 Meoo 的模型能力用 OpenAI 协议调起来」→ **官方 compatible-mode**。凭证可续期、协议标准、无 Cookie 依赖。
- 要「让 agent 帮你建站 / 跑 sandbox / 拿预览地址」→ 只能走 Web 协议，且必须维护 Cookie 会话，还要自己还原 SSE 事件状态机。这部分本项目没做。

一个实测数据点：Web 协议下 `hi` 这一问消耗 4.94 积分、input 29143 tokens（大量 system 注入），
而官方网关的 `1+1=?` 只报 `prompt 59 / completion 14`。**Web 通道的上下文成本高两个数量级**，这也是不选它的现实原因。

---

## 六、前门错误契约（对齐 chat-adapter 规范）

本项目前门遵循 `chat-adapter` 技能的公开错误契约，实现落在 `internal/gateway/errmap.go`。

四条硬约束：

1. 公开体只允许 `{"error":{"message","type","param","code"}}`，顶层只允许 `error`
2. HTTP 402 无条件判额度不足（Meoo 把余额不足包在 `invalid_request_error` 里时，按普通优先级会误判成 400）
3. 结构化 `code`/`type` 先于 `message` 文本；文本只用于分类，绝不复制到输出
4. `param` 只接受 OpenAI 已知顶层字段，嵌套路径一律 `null`

### Meoo 特有的三个码

| 上游码 | 映射 | 说明 |
|---|---|---|
| `MODEL_NOT_SUPPORTED` | 404 `not_found` | 上游把"模型不可用"包在 `invalid_request_error` 里，具体码优先，否则会错报 400 |
| `DataInspectionFailed` | 400 `content_filter` | 阿里内容安全的审核拦截，message 常是 `Input data may contain inappropriate content.` |
| `SERVICE_AK_FORMAT_INVALID` | 401 `invalid_api_key` | AK 前缀不对时的格式校验错误，先于鉴权 |

### 实测对照

| 输入 | 改造前 | 改造后 |
|---|---|---|
| 不存在的模型 | HTTP 200 + 空 body | 400 `invalid_request` |
| 上游 400 带模型清单 | 原文透传 | 400 `not_found` + 固定文案 |
| `n=2` | 400 中文文案 | 400 `unsupported_parameter` `param=n` |
| 流式正常结束 | 两个 `data: [DONE]` | 一个 `[DONE]` |
| 流式无 `finish_reason` | 记 200 成功 | 补 error 帧 + `[DONE]` |

---

## 七、登录与续期：没有 refresh token

抓包时间 2026-11-20，已登录态，刷新首页 + 全量接口探测。

### 结论：Meoo 侧不存在 refresh 接口

| 探测 | 结果 |
|---|---|
| 页面刷新后的 16 个 `/api/` 请求 | 无任何 `refresh` / `token` / `renew` 路径 |
| `GET /api/v1/user/me` 响应头 | 只有 `date`，**无 `Set-Cookie`** |
| `GET https://meoo.com/`（带登录 Cookie） | 只刷新 `__meoo_visit_id__`（12h 访问 ID），**不动 `login_oneday_ticket`** |
| `POST /open/v1/cli-compat/{auth,token,account}/refresh` 等 5 条路径 | 全部 404（统一 `about:blank` 错误信封） |
| `GET /open/v1/cli-compat/account/keys` 等 4 条 AK 管理路径 | 全部 404，**没有 AK 管理/轮换接口** |
| 项目 AK 连续读两次 `cloud/environment` | 同一个值（sha256 一致），**不轮换** |

### 真相：登录是阿里云 SSO 的 OAuth2 code flow

带登录态访问 `GET /api/auth/login_redirect?redirect=…`：

```
HTTP/2 302
location: https://account.aliyun.com/sso/login.htm
            ?response_type=code
            &client_id=oneday
            &redirect_uri=https%3A%2F%2Fmeoo.com%2FaliyunLogin
            &scope=openid&state=…
            &biz_variant=personal
```

- `client_id=oneday`，回调 `/aliyunLogin`，认证完全在 `account.aliyun.com`
- `login_oneday_ticket` 是 Meoo 自己的会话票（一天），**由 SSO 回调签发**
- 假 code 打回调：静默忽略、不 Set-Cookie，说明 code 会被校验
- 未登录调 `/api/v1/user/me` 返回 `{"success":true,"data":null,"redirectUrl":"…/api/auth/login_redirect?…"}`
  前端拿到 `redirectUrl` 再跳转——**续期靠"重新走一次 SSO"，不是靠一个可调 API**

### 三条凭证线的真实生命周期

| 凭证 | 会过期吗 | 怎么续 |
|---|---|---|
| `meoo_ak_…` 个人 AK | 不会（我们签发时 `expiresInDays: 0`） | 不需要；失效就重新授权 |
| `meoo_project_…` 项目 AK | 未观察到轮换（两次读同值） | 不需要；失效重读 `cloud/environment` |
| `login_oneday_ticket` Cookie | **约 1 天** | 只能重新走阿里 SSO；纯 HTTP 拿不到 |

### 对网关的意义

**不需要实现 token refresh。** 纯 HTTP 也无法续 Cookie——那需要 `account.aliyun.com` 域的 SSO 会话 cookie，比 meoo Cookie 更敏感，且属于另一个信任域。

能做的保活只有一种：**定期巡检 + 失效告警**。用个人 AK 调 `account/identity`，401 就把账号标红提示重新接入。

### 补充：官方 CLI 里确实有 refresh token，但那是另一套凭证

下拉 `@aliyun-meoo/cli@0.5.8` 源码（`dist/lib/oauth.js`、`credentials.js`、`apiClient.js`）可以看到两套并存的凭证：

| 类型 | 来源 | 有 refresh 吗 | 能调什么 |
|---|---|---|---|
| `api_key` | `meoo login` 拿到的 `meoo_ak_…` | **没有**（结构里无 `expiresAt`） | 内部接口 + `/open/` |
| `oauth` | `/oauth/authorize` PKCE 授权 | **有**（`refresh_token` + `expires_in`） | **只能调 `/open/`** |

源码里的硬约束：

```js
if (credential.type !== 'api_key' && !path.startsWith('/open/')) {
    throw new ApiRequestError(400, 'OPENAPI_ROUTE_REQUIRED',
        `业务 Access Token 不能调用内部接口 ${path}`);
}
```

OAuth 刷新策略（`credentialProvider.js`）：提前 60 秒刷新（`REFRESH_EARLY_MS`），
收到 `401 invalid_token` 强制刷新，带文件锁防并发。

OAuth 端点（`client_id=meoo_app_tdWlRbpvAxN1EjmZ8XICxdTh`，`scope=cli.compat`）：

```
GET  /oauth/authorize?response_type=code&code_challenge_method=S256&redirect_uri=http://127.0.0.1:{port}/oauth/callback
POST /oauth/token   grant_type=authorization_code | refresh_token（x-www-form-urlencoded）
POST /oauth/revoke  client_id + token
```

响应必须含 `access_token/refresh_token/token_type=Bearer/expires_in/scope/openid/tenant_id`。

**所以：`meoo_ak_` 是"长期 API Key"，OAuth 是"短期访问令牌"。
本网关用前者——它不会过期，也就不需要 refresh；也不需要为了续期去实现 PKCE 回调服务器。**

### 项目 AK 的自愈路径

项目 AK 存在**项目云环境变量**里，不轮换。所以"保养"的正确动作不是刷新，是**重读**：

```
GET /open/v1/cli-compat/projects/{url_id}/cloud/environment
  → data.meooProjectApiKey
```

网关把它做成 `RepairProjectKey`：探活失败 → 重读 → 值变了就原地换血（保留统计），值没变就清掉失效标记。
真实账号实测返回 `{"changed":false,"message":"云端值与本地一致"}`，说明 AK 本身是健康的。
