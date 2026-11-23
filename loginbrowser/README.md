# login-browser

meoo2api 的**服务端浏览器登录** sidecar：用真浏览器把阿里云 SSO 登录走完，把 meoo 的 Cookie 交给主服务。

## 为什么要有这个东西

meoo 的登录只有两条路，且都在 `passport.aliyun.com/havanaone` 上：

| 方式 | 纯 HTTP 复刻的成本 |
|---|---|
| 手机号 + 短信 | 需要 `umidToken`（UMID 设备指纹）+ `csessionid`（无痕验证） |
| 账密 | 在上述基础上再加 RSA-2048 密文 + `_csrf` |

`window.viewData.baxiaApiList` 把 **9 个登录接口全部**挂进了阿里霸下风控，包括 `sms/sendSms.do`。
所以这里一条风控逻辑都不复刻——**风控看到的就是一个真浏览器**，短信/账密都由页面自己完成。

## 与主服务的关系

刻意做成独立进程、独立模块（自带 `go.mod`）：

- 主服务保持**零第三方依赖 + alpine 零 apk**，不被 Chromium 拖成 300MB 级镜像
- Chromium 崩溃不会带走网关；浏览器会话可以单独限额、单独重启
- 只监听回环，主服务按需代理；Cookie 取出后走既有的 `POST /admin/api/cookie/attach` 链路
  （个人 AK → 建项目 → 开云 → 读项目 AK 入池），开号逻辑一行都不用改

## HTTP API

除 `/healthz` 外都需要 `X-Login-Key`（配置了 `LOGIN_KEY` 时）；未配置时只允许回环访问。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 存活与会话数 |
| POST | `/v1/sessions` | `{"mode":"sms"\|"pwd"}` → `{id,status}`；`status=ready` 表示表单可交互 |
| GET | `/v1/sessions/{id}/screen` | 当前画面 PNG（面板轮询这张图） |
| GET | `/v1/sessions/{id}/state` | `{status, page:{url,title,text,checkcode_visible,send_btn}}` |
| POST | `/v1/sessions/{id}/action` | 见下表 |
| POST | `/v1/sessions/{id}/harvest` | 回放 SSO → 取 meoo Cookie；成功返回 `cookies` 头字符串 |
| DELETE | `/v1/sessions/{id}` | 关会话、删临时 profile |

### action 取值

| type | 作用 | 落点 |
|---|---|---|
| `phone` | 填手机号 | `#fm-sms-login-id` |
| `code` | 填短信验证码 | `#fm-smscode` |
| `account` | 填阿里云账号 | `#fm-login-id` |
| `password` | 填密码 | `#fm-login-password` |
| `getcode` | 点「获取验证码」 | `.send-btn-link` |
| `agree` | 勾协议（默认已勾） | `#fm-agreement-checkbox` |
| `submit` | 点登录 | 按文字匹配「登录 / 注册」或「登录」 |

`getcode` 的返回里 `page.checkcode_visible=true` 表示**图片验证码弹出来了**——那是风控真正介入的信号，
此时需要人工识别 `#fm-login-checkcode` 再提交。

## 本地跑

```bash
cd loginbrowser
go build -o /tmp/login-browser .
PORT=8792 BIND=127.0.0.1 /tmp/login-browser

curl -s -X POST localhost:8792/v1/sessions -d '{"mode":"sms"}'
# → {"id":"…","status":"ready"}
curl -s localhost:8792/v1/sessions/<id>/screen -o /tmp/shot.png
curl -s -X POST localhost:8792/v1/sessions/<id>/action -d '{"type":"phone","value":"138…"}'
curl -s -X POST localhost:8792/v1/sessions/<id>/action -d '{"type":"getcode"}'   # ⚠️ 真的会发短信
curl -s -X POST localhost:8792/v1/sessions/<id>/action -d '{"type":"code","value":"123456"}'
curl -s -X POST localhost:8792/v1/sessions/<id>/action -d '{"type":"submit"}'
curl -s -X POST localhost:8792/v1/sessions/<id>/harvest'
```

环境变量：`CHROME_PATH`（Linux 默认 `chromium`）、`CHROME_PROXY`（不配则强制直连）、
`CHROME_SANDBOX=1`（默认关）、`MAX_SESSIONS`（默认 2）、`SESSION_TTL_SEC`（默认 300）、`LOGIN_KEY`。

## 部署

```bash
docker build -t meoo-login-browser ./loginbrowser
docker run -d --name meoo-login --restart unless-stopped \
  -e LOGIN_KEY=… -e MAX_SESSIONS=2 \
  --network meoo2api_default meoo-login-browser
```

镜像里装 `chromium + 中文字体`，容器内必须 `--no-sandbox`（默认已是）；不对外暴露端口。

## 已验证 / 未验证

已验证（本机 macOS + 真 Chrome）：
- 建会话 → 表单就绪（~2.7s）→ 截图 → 填手机号（回读校验一致）→ 点提交（页面回「请先发送短信验证码」）
- 未登录时 `harvest` 会干净失败并给出当前 URL，不会误判

**未验证**：登录成功后 `harvest` 能否拿到 `login_oneday_ticket`（需要真的收一条短信）。
它是整条链路唯一的假设：登录完回放 SSO URL，靠阿里会话换 meoo 的 code。

## 三个踩过的坑（改代码前先看）

1. **chromedp 的浏览器绑在"首次 Run 用的那个 ctx"上**。若首次 Run 发生在 `context.WithTimeout` 派生出来的
   子 ctx 里，调用方一 `cancel()`，浏览器就被关掉——表现为 navigate 看似成功、之后全是 `context canceled`。
   所以 `launchBrowser` 里必须先用长生命周期 ctx 预热一次 `about:blank`。
2. **落地页判定必须比 host**。登录页 query 里本来就带 `redirect_uri=https://meoo.com/aliyunLogin`，
   用 `strings.Contains(url,"meoo.com")` 会把没登录误判成已到 meoo。
3. **`Evaluate[string]` 只接受 JS 字符串**。`!!el` 返回布尔，解码会失败；写成 `String(!!el)`。
   同理按钮文字要两边都去空白再比（页面是「登录 / 注册」）。
