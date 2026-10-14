#!/usr/bin/env bash
#
# LLM-API 服务重启脚本（在 /usr/local/code/llm-api 下执行）
# 功能：重建容器以应用配置修改，不拉代码、不重新编译镜像
#      （相比 deploy.sh 更快，适合只改了 .env / compose 配置的场景）
#
# 用法：
#   ./restart.sh              # 重启 llm-router
#   ./restart.sh --all        # 重启全部服务（当前只有 llm-router）
#
# 其他可选环境变量：
#   CONSOLE_PORT   健康检查端口（默认 8081）

set -euo pipefail

cd "$(dirname "$0")"

# ---------- 0. 前置检查 ----------
command -v docker >/dev/null 2>&1 || { echo "[错误] 未找到 docker"; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "[错误] 未找到 docker compose 插件"; exit 1; }

CONSOLE_PORT="${CONSOLE_PORT:-8081}"

if ! docker network inspect skillbook_default >/dev/null 2>&1; then
  echo "[错误] 未找到网络 skillbook_default，请先启动 skillbook：cd ../skillbook && ./deploy.sh"
  exit 1
fi
if ! docker ps --format '{{.Names}}' | grep -qx 'skillbook-mongo-1'; then
  echo "[错误] skillbook mongo 容器未运行，请先启动 skillbook：cd ../skillbook && ./deploy.sh"
  exit 1
fi

# ---------- 1. 解析要重启的服务 ----------
if [ "$#" -eq 0 ] || [ "$1" = "--all" ]; then
  SERVICES=(llm-router)
else
  SERVICES=("$@")
fi

echo "==> [1/3] 重建并启动容器（不重新编译镜像）：${SERVICES[*]}"
# up -d 会用现有镜像重建容器，从而让 .env 等配置修改生效；
# 不会执行 docker build，因此比 deploy.sh 快很多。
docker compose up -d --force-recreate "${SERVICES[@]}"

echo "==> [2/3] 当前容器状态"
docker compose ps

echo "==> [3/3] 健康检查 (http://localhost:${CONSOLE_PORT}/console/)"
sleep 3
HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://localhost:${CONSOLE_PORT}/console/" || true)"
if [ "$HTTP_CODE" = "200" ]; then
  echo "健康检查通过 ✔"
else
  echo "[警告] 健康检查返回 $HTTP_CODE，请查看日志：docker compose logs -f llm-router"
fi

echo ""
echo "重启完成。查看日志：docker compose logs -f llm-router"
