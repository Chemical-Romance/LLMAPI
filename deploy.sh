#!/usr/bin/env bash
#
# LLM-API 独立部署脚本（在 /usr/local/code/llm-api 下执行）
# 功能：拉取最新代码 -> 拉取 GHCR 预构建镜像 -> 启动容器 -> 健康检查
#       （镜像在本地开发机 ./release.sh 构建并 push，本机零编译）
#
# 用法：
#   ./deploy.sh
#
# 前置依赖：
#   skillbook 项目已启动（提供 mongo 容器与 skillbook_default 网络）
#
# 其他可选环境变量：
#   CONSOLE_PORT   健康检查端口（默认 8081）

set -euo pipefail

cd "$(dirname "$0")"

# ---------- 0. 前置检查 ----------
command -v docker >/dev/null 2>&1 || { echo "[错误] 未找到 docker"; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "[错误] 未找到 docker compose 插件"; exit 1; }
[ -d .git ] || { echo "[错误] 请在仓库目录（含 .git）下执行"; exit 1; }

CONSOLE_PORT="${CONSOLE_PORT:-8081}"

# B 方案依赖 skillbook 的 mongo 容器与网络，缺失时明确提示
if ! docker network inspect skillbook_default >/dev/null 2>&1; then
  echo "[错误] 未找到网络 skillbook_default，请先启动 skillbook：cd ../skillbook && ./deploy.sh"
  exit 1
fi
if ! docker ps --format '{{.Names}}' | grep -qx 'skillbook-mongo-1'; then
  echo "[错误] skillbook mongo 容器未运行，请先启动 skillbook：cd ../skillbook && ./deploy.sh"
  exit 1
fi

echo "==> [1/4] 拉取 GitHub 最新代码 (SSH: $(git config --get remote.origin.url || true))"
# remote 使用 SSH（git@github.com:...）；分支未设上游时显式指定 origin/<当前分支>
# 本机到 GitHub 的网络可能不稳定：最多重试 3 次，仍失败只警告，继续部署本地现有代码
BRANCH="$(git symbolic-ref --short HEAD || echo master)"
pull_ok=0
for i in 1 2 3; do
  if git pull origin "$BRANCH"; then pull_ok=1; break; fi
  echo "[警告] 第 $i 次拉取失败，3 秒后重试..."
  sleep 3
done
if [ "$pull_ok" -ne 1 ]; then
  echo "[警告] 拉取 GitHub 失败（网络不通？），继续部署本地现有代码"
else
  echo "代码已是最新"
fi

echo "==> [2/4] 拉取 GHCR 预构建镜像并启动容器（正常情况本机零编译）"
# 镜像由本地开发机 ./release.sh 构建并 push 到 ghcr.io（本地编译，服务器只拉）。
# 本机 2 vCPU / 3.6G，Go 编译虽不重但同样避免；网络不稳：最多重试 3 次。
pull_images() {
  if command -v ionice >/dev/null 2>&1; then
    nice -n 19 ionice -c3 docker compose pull llm-router
  else
    docker compose pull llm-router
  fi
}
pull_ok=0
for i in 1 2 3; do
  if pull_images; then pull_ok=1; break; fi
  echo "[警告] 第 $i 次拉取镜像失败，3 秒后重试..."
  sleep 3
done

if [ "$pull_ok" -ne 1 ]; then
  if docker image inspect "$(docker compose config --images | grep llm-api)" >/dev/null 2>&1; then
    echo "[警告] GHCR 拉取失败，回退使用本地已有旧镜像"
    echo "        （若是 401/403，需先登录：docker login ghcr.io，用 GitHub PAT，权限 read:packages）"
  else
    echo "[警告] GHCR 拉取失败且本地无镜像，回退本地限流构建（较慢）"
    if command -v ionice >/dev/null 2>&1; then
      COMPOSE_PARALLEL_LIMIT="${COMPOSE_PARALLEL_LIMIT:-1}" nice -n 19 ionice -c3 docker compose --parallel 1 up -d --build
    else
      COMPOSE_PARALLEL_LIMIT="${COMPOSE_PARALLEL_LIMIT:-1}" docker compose --parallel 1 up -d --build
    fi
  fi
fi
# up -d 不带 --build：镜像已存在时直接起容器，不触发编译
docker compose up -d

echo "==> [3/4] 当前容器状态"
docker compose ps

echo "==> [4/4] 健康检查 (http://localhost:${CONSOLE_PORT}/console/)"
sleep 3
HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://localhost:${CONSOLE_PORT}/console/" || true)"
if [ "$HTTP_CODE" = "200" ]; then
  echo "健康检查通过 ✔"
else
  echo "[警告] 健康检查返回 $HTTP_CODE，请查看日志：docker compose logs -f llm-router"
fi

echo ""
echo "部署完成。查看日志：docker compose logs -f llm-router"
