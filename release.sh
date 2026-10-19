#!/usr/bin/env bash
#
# LLMAPI 镜像发布脚本 —— 在【本地开发机】执行，不在服务器上跑。
# 功能：构建 linux/amd64 镜像并推送到 GHCR，服务器侧由 ./deploy.sh 只做
#       docker compose pull，不再本机编译。
#
# 前置条件（一次性）：
#   1. 本地已安装 Docker（Desktop 或 Engine 均可）
#   2. docker login ghcr.io   # GitHub 账号 + PAT（权限需 write:packages）
#   3. 代码已提交并推送（镜像 tag 用 git 短 SHA，便于回滚）
#
# 用法：
#   ./release.sh                # 构建并推送 latest + git 短 SHA 两个 tag
#   ./release.sh --no-push      # 只构建不推送（本地验证用）
#
# 服务器回滚到某个版本：
#   IMAGE_TAG=<短SHA> ./deploy.sh   （见 docker-compose.yml 的 image 字段）

set -euo pipefail
cd "$(dirname "$0")"

# ---------- 0. 前置检查 ----------
command -v docker >/dev/null 2>&1 || { echo "[错误] 本地未安装 docker"; exit 1; }

if ! grep -q 'ghcr.io' "${HOME}/.docker/config.json" 2>/dev/null; then
  echo "[错误] 尚未登录 GHCR。请先执行："
  echo "       docker login ghcr.io"
  echo "       （GitHub 用户名 + PAT，权限需 write:packages；PAT 在 GitHub → Settings → Developer settings 生成）"
  exit 1
fi

BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || echo unknown)"
DIRTY=""
if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
  DIRTY="-dirty"
  echo "[警告] 工作区有未提交改动，镜像将带 -dirty 后缀，线上代码与镜像可能不一致"
fi
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"

PUSH=1
if [ "${1:-}" = "--no-push" ]; then PUSH=0; fi

# 服务器是 x86_64(linux/amd64)，Apple Silicon 本地必须显式指定
PLATFORM="${PLATFORM:-linux/amd64}"
OWNER="ghcr.io/chemical-romance"
TAGS=("${OWNER}/llm-api:latest" "${OWNER}/llm-api:${SHA}${DIRTY}")

BUILD_ARGS=(--platform "$PLATFORM" --progress=plain)
for t in "${TAGS[@]}"; do BUILD_ARGS+=(-t "$t"); done

# ---------- 1. 构建 ----------
echo "==> [1/2] 构建镜像 ($PLATFORM)，分支 $BRANCH @ $SHA"
docker build "${BUILD_ARGS[@]}" .

# ---------- 2. 推送 ----------
if [ "$PUSH" -eq 1 ]; then
  echo "==> [2/2] 推送到 GHCR"
  for t in "${TAGS[@]}"; do docker push "$t"; done
  echo ""
  echo "发布完成 ✔  镜像："
  printf '  %s\n' "${TAGS[@]}"
  echo "服务器部署：cd /usr/local/code/deploy && ./deploy.sh llm-api"
  echo "回滚：      服务器上 IMAGE_TAG=<短SHA> ./deploy.sh llm-api"
else
  echo "==> [2/2] --no-push，跳过推送（本地验证完成）"
fi
