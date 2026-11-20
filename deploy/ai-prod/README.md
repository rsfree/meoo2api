# ai-prod 部署（meoo2api.1task.cn）

push 到 `main` 后由 `release` → `deploy` 两个 job 自动完成：
发版 → 推 GHCR → SSH 调 `/opt/meoo2api/update.sh <version>` → 公网验收。

## 服务器侧布局

```
/opt/meoo2api/
├── docker-compose.yml    # 端口只绑 127.0.0.1:8791
├── update.sh             # pull + up + 健康检查 + 版本断言
└── data/                 # 10001:10001，data.json + requests.jsonl
```

nginx：
- `/www/server/panel/vhost/nginx/meoo2api.1task.cn.conf`（80，除 ACME 外 301）
- `/www/server/panel/vhost/nginx/meoo2api.1task.cn-ssl.conf`（443，`proxy_buffering off` 保证 SSE 不积压）
- 证书 `/www/server/panel/vhost/cert/meoo2api.1task.cn/`（acme.sh，自动续期）

## 手工操作

```bash
# 回滚到某个版本
ssh root@210.121.44.245 '/opt/meoo2api/update.sh 0.0.4'

# 看日志（首次启动的一次性密码在这里）
ssh root@210.121.44.245 'docker logs meoo2api | head -3'

# 重发 vhost
scp nginx/*.conf root@210.121.44.245:/www/server/panel/vhost/nginx/ \
  && ssh root@210.121.44.245 '/www/server/nginx/sbin/nginx -t && /www/server/nginx/sbin/nginx -s reload'
```

## CI 用到的 secrets

| 名称 | 内容 |
|---|---|
| `AI_PROD_SSH_KEY` | 专用 ed25519 私钥（注释 `meoo2api-ci-deploy`），只用于本服务 |
| `AI_PROD_HOST` | `210.121.44.245` |

撤销：从 `/root/.ssh/authorized_keys` 删掉对应行，并删除仓库 secret。

## 已知限制

镜像只发 `linux/amd64`，而 ai-prod 正是 x86_64，无影响。
