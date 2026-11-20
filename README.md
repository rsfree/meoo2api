# meoo2api

把 **秒悟 Meoo**（`meoo.com`）账号包装成 OpenAI 兼容接口的自托管网关，附一个运维级 Web 控制台。

Go 单二进制，零第三方依赖，前端编进二进制，部署只需要一个可执行文件 + 一个数据目录。

```
OpenAI 客户端 → http://host:8791/v1
                      ↓ 号池里任选一个项目 AK
https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 为什么这样接

Meoo 前端自己走的是 `/api/agent/start` + `/api/agent/query`（SSE）的 agent 协议，要网页 Cookie、要 sandbox、事件要自己拼。但它同时提供了一条**官方 OpenAI 兼容网关**：`https://api.meoo.host/meoo-ai/compatible-mode/v1`，用**项目服务 AK**（`meoo_project_…`）鉴权，协议就是标准 OpenAI。

所以本项目的定位不是"逆向聊天流"，而是**凭证运营**：

| 层 | 作用 |
|---|---|
| 设备授权（`/api/v1/cli/auth/session`） | 浏览器点一次确认，换到个人 AK `meoo_ak_…`，无需 Cookie、无需浏览器自动化 |
| 项目开通（`/open/v1/cli-compat/projects`） | 用个人 AK 建项目 → 开通云服务 → 读环境变量拿项目 AK |
| 号池 | 项目 AK 入池，做选号、冷却、熔断、换号 |
| 前门 | 只对外暴露 `/v1/models`、`/v1/chat/completions` |

三种凭证互相独立，别混：

| 凭证 | 能干什么 |
|---|---|
| `meoo_ak_…`（设备授权） | 身份、建项目、开云服务、查积分；**不能**调模型 |
| `meoo_project_…`（项目服务） | **唯一**能调模型网关的凭证，进号池 |
| 网页 Cookie | 只在网页端签到/保活时有用；本项目的接入流程完全不需要 |

## 两种加号方式

| 方式 | 需要什么 | 适用 |
|---|---|---|
| **浏览器授权** | 点一次链接确认 | 常规；授权页偶发被 SPA 首页接走 |
| **粘贴 Cookie** | 从浏览器复制一段 Cookie | 授权页渲染不出来时；或已有账号补登录态 |

Cookie 方式的链路（全 HTTP，验证过）：

```
Cookie → GET /api/v1/user/me 确认身份
       → POST /api/v1/cli/auth/session 建会话
       → POST /api/v1/cli/auth/approve（带 Cookie，等于替你点"授权登录"）
       → 轮询会话拿个人 AK → 建项目 → 开云 → 项目 AK 入池
```

去重逻辑：同一 `user_id` 若已有可用项目 AK，**只更新 Cookie 与个人 AK，不重复建项目**；上次开通中断的半程账号会被复用而不是新建。

Cookie 是敏感凭证：只在面板登录后可提交，以 `0600` 存本地，接口响应与日志都不回显（有测试断言）。

⚠️ 注意 Cookie 里的 `login_oneday_ticket` / `oneday_sid` 名字已经说明生命周期——**大约一天就会失效**。
所以它适合"一次性接入"，不适合长期保活；账号的长期可用性靠个人 AK + 项目 AK，与 Cookie 无关。

## 快速开始

```bash
go build -o bin/meoo2api ./cmd/server
./bin/meoo2api -port 8791 -data data
```

首次启动会打印一次性管理密码（`data/data.json` 里存的是 PBKDF2 哈希）。

打开 `http://127.0.0.1:8791/` → 登录 → **账号 → 添加账号**：

1. 弹窗给出授权链接 `https://meoo.com/cli/login?session_id=…`
2. 在你的浏览器里登录 Meoo 并确认授权（15 分钟内有效）
3. 服务端轮询到 `approved`，自动建项目、开云服务、把项目 AK 入池

> 授权页如果被踢回首页，先登录 Meoo，再重新打开弹窗里的链接。

## API

用控制台「凭证」页签发的 `sk-…`，也可以直接拿项目 AK 当 Bearer（BYOK，不进号池计数）。

```bash
curl http://127.0.0.1:8791/v1/chat/completions \
  -H 'Authorization: Bearer sk-YOUR_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen","messages":[{"role":"user","content":"你好"}]}'
```

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 流式 / 非流式；`n` 只支持 1 |
| GET | `/v1/models`、`/v1/models/{id}` | 上游目录 + 本地别名 |
| GET | `/healthz` | 进程与可用凭证数 |
| GET | `/` | 控制台 |

未实现的 OpenAI 端点一律返回 404 并说明原因，不会假装成功。

### 出站整形

- `model` 走别名解析（默认 `qwen=qwen3.8-flash`、`ds=deepseek-v4-flash-0731`、`kimi=kimi-k3`）
- `max_completion_tokens` 归一成 `max_tokens`
- 客户端没给 `max_tokens` 时**不注入**（`default_max_tokens=0` 跟随上游），避免思考模型的预算被 800 掐死
- 非流式请求丢弃 `stream_options`
- `n != 1` 直接报错，不做本地并发模拟

### 错误与换号

| 上游 | 处置 |
|---|---|
| `403 QUOTA_EXCEEDED` / 余额不足 | 该 AK 冷却 30 分钟（可配），请求换下一个号 |
| `401/403` 鉴权失败 | 标记 `invalid`，不再选它，等人工探活或重新开通 |
| `429` | 冷却 10 分钟（可配） |
| `5xx` | 冷却 60 秒 |
| `400` 等参数错误 | **不换号**（换号只会重复计费），原样返回 |
| 客户端断开 | 不处罚凭证 |

换号仅发生在**响应头还没发出**时，最多 4 次。

### 流式

- 上游静默超过 `keepalive_sec`（默认 12s）补 `: keepalive` 注释帧，防止长思考被客户端判超时
- 心跳与业务帧走同一条写路径，不会把一帧劈成两半
- 上游没给 `finish_reason` 就断流，日志记为 `502`，不伪装成正常结束

## 错误契约（对齐 chat-adapter 规范）

前门所有失败都走同一张固定文案表，**不存在"上游说什么就转什么"的路径**：

| HTTP | type | code | 场景 |
|---:|---|---|---|
| 400 | `invalid_request_error` | `invalid_request` | 请求格式/参数错误 |
| 400 | `invalid_request_error` | `context_length_exceeded` | 上下文超限 |
| 400 | `invalid_request_error` | `unsupported_parameter` | 参数不支持（如 `n>1`） |
| 400 | `invalid_request_error` | `content_filter` | 内容安全拦截 |
| 401 | `authentication_error` | `invalid_api_key` | 鉴权失败 |
| 403 | `permission_error` | `permission_denied` | 权限不足 |
| 404 | `not_found_error` | `not_found` | 模型/资源不存在 |
| 429 | `rate_limit_error` | `insufficient_quota` | 余额或额度不足 |
| 429 | `rate_limit_error` | `rate_limit_exceeded` | 限流，可退避 |
| 502 | `api_error` | `connection_error` | 连不上上游 |
| 502 | `api_error` | `stream_interrupted` | 流缺语义终态 |
| 502 | `api_error` | `upstream_error` | 安全兜底 |
| 503 | `server_error` | `service_unavailable` | 服务暂不可用 |
| 504 | `timeout_error` | `upstream_timeout` | 上游超时 |

判定顺序（契约固定）：

```
HTTP 402 → 无条件额度不足（上游常夹带 invalid_request_error，先于内层码）
      ↓
结构化 code/type：具体码 → OpenAI → DeepSeek → Ark → Kimi
      ↓
message 短语（只用于分类，绝不进公开文案）
      ↓
HTTP 状态兜底 → 502 upstream_error
```

**脱敏**：`message` 一律取固定文案，`param` 只接受 OpenAI 已知顶层字段（`messages[0].content` 这类嵌套路径置 `null`），上游 provider、原始业务码、request ID、账号邮箱全部只进内部日志。实测：上游返回的「当前仅支持配置模型: qwen3.8-flash、…」不会出现在客户端响应里。

**流式失败**：首帧前失败仍可换号；首帧后失败只能补一帧公开 `error` 再补 `[DONE]`，不伪造成功。`[DONE]` 会被延迟到收尾统一写出，避免"只有 `[DONE]` 没有 `finish_reason`"被当成正常结束。

**重试边界**：`Retry-After` 只在内部判定可重试的规格上输出（固定整数秒）。超时和连接中断默认不自动重试——上游可能已经受理，重放就是重复计费。

## 控制台

五个视图：

- **概览** — 可用凭证 / 积分合计 / 成功率 / 平均 TTFB，上游目录状态，一键同步，在线调试
- **账号** — 每个身份一张卡：积分、个人 AK（掩码）、项目、凭证状态灯、冷却倒计时、刷新积分 / 重新开通 / 停用 / 删除
- **凭证** — 客户端 `sk-` 管理（签发只显示一次）+ 号池项目 AK 列表（探活 / 解除冷却 / 删除）
- **日志** — 每请求一行元数据（模型、状态、TTFB、耗时、tokens、来源 IP），**不落提示词与响应正文**
- **设置** — 默认 max_tokens、超时、冷却时长、心跳间隔、自动换号开关、模型别名、改密码，保存即生效

## 配置

环境变量（`config.example.env`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `BIND` / `PORT` | `127.0.0.1` / `8791` | 监听地址 |
| `DATA_DIR` | `data` | 数据目录（`data.json` + `requests.jsonl`） |
| `MEOO_AI_BASE` | `https://api.meoo.host/meoo-ai/compatible-mode/v1` | 上游网关 |
| `MEOO_PROJECT_AK` | — | 首启预置一个项目 AK |
| `MEOO2API_KEYS` | — | 首启预置客户端密钥，逗号分隔 |

## 冒烟测试

```bash
python3 scripts/smoke.py --base http://127.0.0.1:8791 --key sk-…
```

只用标准库、禁用系统代理，真实打上游。覆盖：健康与目录、鉴权、5 类参数校验、上游错误映射与脱敏、
非流式/多轮/max_tokens 截断、流式增量与 `[DONE]` 终态、tools 透传。实测 21 项：通过 20、跳过 1。

跳过的那项本身就是个发现：**推理模型的思考 token 也计入 `max_tokens`**。
给 `deepseek-v4-*` 设 `max_tokens: 32`，思考就把预算烧完了，`content` 为空、`finish_reason: length`。
网关不改写这种响应（忠诚透传），但会在日志里标注：

```
思考已耗尽 max_tokens，正文为空；推理模型请把预算调到 512 以上
```

## 凭证保养：AK 不会过期，只需要"重读"

Meoo 有两套凭证，官方 CLI 源码（`@aliyun-meoo/cli`）里写得很清楚：

| 类型 | 有过期时间吗 | 能调什么 |
|---|---|---|
| `api_key`（`meoo_ak_…`，本网关用的） | **没有** | 内部接口 + `/open/` |
| `oauth`（`/oauth/authorize` PKCE） | 有（`expires_in` + `refresh_token`） | **只能** `/open/` |

`api_key` 是长期 Key，结构里根本没有 `expiresAt` 字段——所以**不需要 refresh**，也不需要为了续期去实现 PKCE 回调服务器。

真正要保养的是**项目 AK**：它存在项目云环境变量里、不轮换，失效时的正确动作是**重读**而不是刷新：

```
GET /open/v1/cli-compat/projects/{url_id}/cloud/environment → data.meooProjectApiKey
```

网关据此做了三层：

1. **自愈**（`RepairProjectKey`）：探活失败就重读；值变了原地换血（保留 ID 与统计），值没变说明是误判，清掉失效标记
2. **巡检**（`HealthLoop`，每小时）：用个人 AK 探 identity，**只有 401/403 才标红**，网络抖动不改状态
3. **手动兜底**：面板凭证行的「探活」「重读云端」，账号卡的「重读凭证」

真实账号实测：`{"changed":false,"message":"云端值与本地一致"}` —— AK 健康，链路正常。

## 持久化与安全

- `data.json` 权限 `0600`，写入走「临时文件 → fsync → rename」，目录 `fsync` 后返回
- 数据目录加 `flock`，同一目录只允许一个写进程
- 状态文件损坏时**保留原文件并报错退出**，不自动清空账号
- 管理密码 PBKDF2-SHA256（21 万轮随机盐），登录成功后自动升级旧格式
- 管理 API 只接受同源请求（CSRF），会话 HttpOnly + SameSite=Strict，24 小时过期
- 面板 CSP `default-src 'none'`，凭证一律掩码返回，完整值只在签发时显示一次

## 目录

```
cmd/server/         启动、路由、优雅退出、换号状态包装
internal/model/     数据结构与凭证形态
internal/store/     原子持久化、目录锁、密码哈希
internal/upstream/  上游 HTTP、错误分类（quota/auth/rate/server/bad_request）
internal/account/   设备授权、项目开通、身份与积分
internal/pool/      选号、冷却、探活
internal/catalog/   模型目录同步（5 分钟）、别名解析、失败回退
internal/gateway/   /v1 前门、流式转发、心跳、日志
internal/admin/     管理 API、会话与节流
internal/reqlog/    请求日志环形 + JSONL 滚动
internal/webui/     单页控制台（go:embed）
docs/PROTOCOL.md    两条通道的抓包记录与上游接口清单
```

## 发版

push 到 `main` 自动递增 patch 并发版（`.github/workflows/release.yml`，零第三方 action）：

- 版本号取最新 tag +1；仓库零 tag 时从 `v0.0.1` 起（冷启动回落 `v0.0.0`）
- 交叉编译 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`
- `dist/` 下产出二进制 + `SHA256SUMS`，全部挂到 GitHub Release
- 移动 `latest` tag 指向新版本
- 版本号通过 `-ldflags -X main.version=` 注入，`/healthz` 会自报（可用来核对镜像/二进制与 tag 是否一致）

**逃生阀**：commit message 含 `[skip release]` 时整条流水线跳过，适合纯文档提交。
注意它匹配的是整条 message，连"给逃生阀写说明"的提交自己也会命中——中招时用
`gh workflow run release.yml --ref main` 补跑。

### 发版链路实测

| 步骤 | 结果 |
|---|---|
| 首次 push `main` | `v0.0.1`，5 个平台二进制 + `SHA256SUMS` 挂上 Release |
| tag 解引用 | `v0.0.1^{}` == 触发提交（annotated tag 的 `object.sha` 是 tag 对象，别误判） |
| `latest` | 指向同一 commit |
| 二进制自报版本 | 下载 `darwin-arm64` 跑 `/healthz` → `"version":"v0.0.1"` |
| 逃生阀 | 提交 message 带标记 → run `skipped`，无新版本 |
| patch 自增 | 下一正常提交 → `v0.0.2` |

## 合规

仅用于**你自己授权过的 Meoo 账号**在私有环境自用。批量注册、转售接口、绕过平台配额都违反上游服务条款，本项目不为这类用法提供任何支持。

---

## 实测记录（2026-11-20）

一次完整的真实闭环，全部经过验证：

| 步骤 | 结果 |
|---|---|
| 创建设备授权会话 | `POST /api/v1/cli/auth/session` → `sessionId` |
| 浏览器确认授权 | 调用 `POST /api/v1/cli/auth/approve`（`sessionId` + `keyName` + `expiresInDays`）→ 返回 `keyPrefix: meoo_ak_…` |
| 服务端轮询 | `status: approved`，拿到个人 AK |
| 身份校验 | `user_id` 与昵称均正常返回（此处不落真实值） |
| 建项目 + 开云 + 读 AK | 项目 `culsdhftah2f`，服务自动开通，项目 AK 入池 |
| 积分 | 6,968 |
| 模型目录 | 10 个真实模型（`qwen3.8-flash` / `qwen4-max` / `qwen3.8-max` / `qwen3.7-max` / `qwen3.7-plus` / `deepseek-v4.1-flash-1017` / `deepseek-v4-pro-0813` / `deepseek-v4-flash-0731` / `glm-5.2` / `glm-5.2-fast-preview`） |
| 非流式对话 | `qwen`（别名 → `qwen3.8-flash`）返回「网关已通」，`usage` 131 tokens |
| 流式对话 | `deepseek-v4-flash-0731` SSE 正常，`delta.reasoning_content` 透传，以 `data: [DONE]` 收尾 |
| 日志 | `ttfb 3760ms / dur 3761ms / 47 tokens`，归属项目 `culsdhftah2f` |

### 抓包过程中修掉的三个真实 bug

1. **模型目录形态判断错**：官方网关返回的是 `{"models":["qwen3.8-flash",…]}`（**字符串数组**），而 Web 端是 `{"data":{"models":[{key,value}]}}`（对象数组）。只按对象解析会报 `cannot unmarshal string`，目录永远是空的。
2. **HTTP 200 业务失败被当成功**：Meoo 部分接口用 `200 + {"success":false,"errCode":…}` 表达失败。原实现只看状态码，会把错误数据透给客户端。现在 2xx 也过一遍失败信封，并按 `errCode` 归类到 quota/auth/rate。
3. **TTFB 统计恒为 0**：流式路径算了首帧时间却没传给日志函数，面板"平均首字节"永远是 0。

---

## 实测能力矩阵（2026-11-20 真实压测）

### 多轮对话：支持，但是**无状态多轮**

网关不做会话存储，每次请求必须带完整 `messages` 历史（标准 OpenAI 语义）。

实测 8 轮累积：`prompt_tokens` 从 75 线性增长到 278，回问第 1 轮的暗号仍能准确召回（`'1000, 1001'`）。
**上下文确实延续，靠的是客户端把历史传回来**——不是上游记着。

### 上下文长度：标称 200k，实测 280k 通过

| 输入规模 | prompt_tokens | 耗时 | 结果 |
|---:|---:|---:|---|
| 8.6k 字 | 4,446 | 20.0s | ✓ |
| 34.7k 字 | 17,577 | 10.0s | ✓ |
| 30 万字 | 108,985 | 9.7s | ✓ |
| 40 万字 | 144,140 | 12.1s | ✓ |
| 55 万字 | 198,114 | 21.9s | ✓ |
| 70 万字 | 250,561 | 13.2s | ✓ |
| 78 万字 | 280,220 | 10.5s | ✓ |

Web 端标称 `context_window: 200000`，官方网关实测能吃到 280k 还没报错。
**但别贴着上限用**：一次 280k 输入的账单和延迟都不可控。

⚠️ 踩坑：**高度重复的输入会被上游判为垃圾内容**。
用「天地玄黄宇宙洪荒」×880 构造 5k tokens 时，上游返回 `502 SERVICE_UPSTREAM_REQUEST_FAILED: 百炼请求失败`（耗时 62s）。
换成自然文本（README、代码）同样长度立刻通过。所以别用重复串做压力测试，那不是长度问题。

### 模型：12 个全部可用（10 真实 + 2 别名）

| 模型 | 首字延迟 | 备注 |
|---|---:|---|
| `qwen3.8-flash` | 1.6s | 最快，默认 |
| `qwen3.8-max` | 1.8s | |
| `qwen4-max` | 2.2s | |
| `deepseek-v4.1-flash-1017` | 4.4s | 推理 |
| `deepseek-v4-flash-0731` | 4.2s | 推理 |
| `deepseek-v4-pro-0813` | 4.9s | 推理 |
| `glm-5.2` | 5.5s | 推理，64 token 预算会被思考吃光 |
| `glm-5.2-fast-preview` | 6.7s | 同上 |
| `qwen3.7-max` | 7.2s | |
| `qwen3.7-plus` | 8.2s | |

另有别名 `qwen` → `qwen3.8-flash`、`ds` → `deepseek-v4-flash-0731`、`kimi` → `kimi-k3`。

### 并发与频控：96 并发零失败

| 并发 | 成功 | 失败 | 总耗时 | P50 | P95 |
|---:|---:|---:|---:|---:|---:|
| 3 | 3 | 0 | 4.4s | 4.4s | 4.4s |
| 12 | 12 | 0 | 7.0s | 5.4s | 6.0s |
| 24 | 24 | 0 | 8.6s | 7.1s | 7.7s |
| 48 | 48 | 0 | 6.9s | 5.0s | 5.7s |
| 96 | 96 | 0 | 10.9s | 7.4s | 9.0s |

**全程没有一个 429**。上游对单账号的并发几乎没有限制（或做了排队削峰）。
延迟随并发增长温和（P50 4.4s → 7.4s），说明瓶颈在上游推理而非连接。

### 成本：确实扣积分，约 1,100 tokens / 积分

**先纠正一个我自己踩过的坑**：压测后看面板显示 6,968 分，以为"不扣钱"。那是**面板缓存**——
`points` 是接入时查的值，`HealthLoop` 一小时才刷一次。实查 `account/points` 接口：

```
totalGranted  7000   （订阅赠送 5000 + 每日登录 2000）
totalConsumed  702   ← 压测真的扣了
availableBalance 6298
```

网关现在会**自己采样**（每 5 分钟一次，面板可手动触发）：查一次可用积分，用两次快照相减得出
这段时间花掉多少分，再按各模型的 token 增量记进样本。所以单价会随运行时间自动收敛，不用手工做实验。

多次采样后的实测值（qwen3.8-flash）：

```
样本1   8,868 tokens → 2 分
样本2  26,729 tokens → 7 分
汇总   35,597 tokens → 9 分   ≈ 3,955 tokens / 积分
```

→ **100 万 tokens 约 250 积分**，7,000 分总额大约能跑 **2,800 万 tokens**。

⚠️ **单次测量不可信**：积分是整数精度，而且入账有延迟（几秒到十几秒）。
我最早做的单次实验测出 1,108 tok/分，比真实值差 4 倍——因为那一次的快照把**前面压测的延迟入账**一起算进去了。
这就是为什么采样要多次累计、而不是每请求前后各查一次。

面板「概览」有一张「积分成本」卡，账号卡里也标了「已花费 N 分 · M tok · ≈K tok/分」。

面板上每个账号的积分后面会标「N 分钟前」，就是提醒这个数字可能是缓存的；点「刷新积分」才会实查。

> 另外 Meoo 侧把这笔额度叫 `model_credits`（`/api/mc/entitlement/*`，`quota 7000 / used 702`），
> 和网页 agent 用的积分是同源的，只是展示口径不同。

### 多号场景：选号 / 冷却 / 分流实测

**真实链路**（往池里注入一个假 AK 模拟坏号）：

```
注入 meoo_project_FAKEdeadbeef0000 → state=ok
连发 6 次请求 → 6/6 HTTP 200
事后查池：
  真号  state=ok       req=296
  假号  state=invalid  fails=1  reason=SERVICE_AK_FORMAT_INVALID
```

假号只被打了一次就被判死，之后 5 次全走真号。**没有一个请求因为坏号失败。**

**在途租约（防惊群）**：选号时按"在途请求数最少"优先，其次"最久未被选中"，所以并发会自然分摊到多个凭证。

**但惊群无法彻底消除**——这是物理限制，不是实现缺陷：

```
24 并发 + 2 个凭证（1 坏 1 好）
→ 坏号被打 10 次（完全均分是 12 次）
→ 冷却写回后再打 12 次：坏号 0 次
```

前 10 次是"429 的反馈还没回来，请求已经在路上"。能做的只有分摊和快速收敛，不可能提前知道哪个号要挂。

**分工合理的边界**：

| 场景 | 行为 | 理由 |
|---|---|---|
| 上游 429 | 冷却 10 分钟后换号 | 限流是临时的 |
| 上游 401/403 | 标 `invalid`，不再参与选号 | 凭证坏了，换号更好 |
| 上游 5xx | 冷却 60 秒后换号 | 可能是瞬时故障 |
| **上游 400** | **不换号，原样返回** | 换号只会重复计费；实测 4 次请求打 4 次上游，零重试 |
| 全部冷却 | 直接 503，不打上游 | 避免每个客户端都白打一次 |

## 待验证（需要更多账号才能测）

| 项 | 前置条件 | 怎么验 |
|---|---|---|
| **两个好号之间的负载均衡** | 池里 ≥2 个可用项目 AK（需要第二个 Meoo 账号） | 跑 48 并发，断言两条凭证 `requests` 接近 1:1（偏差 <20%），P50 不劣化 |
| **跨号会话粘性** | 同上 | 同一会话连续多轮是否落在同一账号上（当前实现不绑定，每轮独立选号） |
| **单号限流的真实阈值** | 更多并发或更长压测 | 96 并发未触发 429，说明阈值高于此；需要更大压力才能找到 |
