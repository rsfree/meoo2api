#!/usr/bin/env bash
# 由 CI 调用：update.sh <version>（不带 v，例如 0.0.4）
# 手工回滚：update.sh 0.0.3
set -euo pipefail
TAG="${1:?用法: update.sh <version>（0.0.4 或 v0.0.4 均可）}"
TAG="${TAG#v}"   # 镜像 tag 不带 v 前缀
cd "$(dirname "$0")"

echo "==> 目标版本 meoo2api ${TAG}"
IMAGE_TAG="$TAG" docker compose pull meoo2api
IMAGE_TAG="$TAG" docker compose up -d meoo2api

echo -n "==> 等待健康检查"
for _ in $(seq 1 25); do
  if curl -sf -m 3 http://127.0.0.1:8791/healthz >/dev/null 2>&1; then echo " ok"; break; fi
  echo -n "."
  sleep 2
done

# 验收判据写进脚本：版本必须等于目标版本，否则说明镜像没真正换掉
VERSION=$(curl -sf -m 5 http://127.0.0.1:8791/healthz | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')
python3 - "$TAG" "$VERSION" <<'PY'
import sys
want, got = sys.argv[1], sys.argv[2].lstrip("v")
assert want == got, f"!! 版本不符：期望 {want}，实际 {got} —— 镜像没换掉"
print(f"  验收 OK · version={got}")
PY

docker inspect meoo2api --format '  容器 {{.State.Status}} · 镜像 {{.Config.Image}} · {{index .Image | printf "%.19s"}}…'
echo "==> 完成"
