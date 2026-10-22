#!/usr/bin/env bash
#
# LLMAPI 镜像发布脚本 —— 在【本地开发机】执行，不在服务器上跑。
# 功能：本地构建 linux/amd64 镜像 → 通过 SSH 直接传输到服务器并加载 → 服务器重启容器
#       （不使用 GHCR，不需要 docker login）
#
# 前置条件（一次性）：
#   1. 本地已安装 Docker（Desktop 或 Engine 均可）
#   2. 本机 SSH 密钥可免密登录服务器（推荐），或在下方填入 SERVER_PASS 并安装 sshpass
#   3. 代码已提交（镜像 tag 用 git 短 SHA，便于回滚）
#
# 用法：
#   ./release.sh                # 构建 → 传输镜像 → 服务器部署
#   ./release.sh --no-deploy     # 只构建并传输镜像，不重启容器
#   ./release.sh --deploy-only  # 跳过构建，只连服务器重启容器
#
# 服务器回滚到某个版本：
#   IMAGE_TAG=<短SHA> docker compose up -d  （在服务器 /usr/local/code/llm-api 目录）

set -euo pipefail
cd "$(dirname "$0")"

# ============================================================
# 服务器部署配置（仅本机使用，本文件建议加入 .gitignore 不提交）
#   SERVER_SSH   目标主机，例：snowbeasts@172.104.64.58 或 ssh 别名
#   SERVER_DIR   服务器上的仓库/部署目录，例：/usr/local/code/llm-api
#   SERVER_PORT  SSH 端口（默认 22）
#   SERVER_PASS  密码登录时填写（需安装 sshpass）；留空 = 密钥认证
# ============================================================
SERVER_SSH=""
SERVER_DIR="/usr/local/code/llm-api"
SERVER_PORT="22"
SERVER_PASS=""

# ---------- 参数解析 ----------
NO_DEPLOY=0
DEPLOY_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --no-deploy)   NO_DEPLOY=1 ;;
    --deploy-only) DEPLOY_ONLY=1 ;;
    *)
      echo "[错误] 未知参数：$arg"
      echo "用法：./release.sh [--no-deploy|--deploy-only]"
      exit 1
      ;;
  esac
done
if [ "$NO_DEPLOY" -eq 1 ] && [ "$DEPLOY_ONLY" -eq 1 ]; then
  echo "[错误] --no-deploy 与 --deploy-only 不能同时使用"
  exit 1
fi

# ---------- 0. 前置检查 ----------
command -v docker >/dev/null 2>&1 || { echo "[错误] 本地未安装 docker"; exit 1; }
command -v ssh >/dev/null 2>&1 || { echo "[错误] 未找到 ssh"; exit 1; }

BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || echo unknown)"
DIRTY=""
if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
  DIRTY="-dirty"
  echo "[警告] 工作区有未提交改动，镜像将带 -dirty 后缀，线上代码与镜像可能不一致"
fi
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"

# 服务器是 x86_64(linux/amd64)，Apple Silicon 本地必须显式指定
PLATFORM="${PLATFORM:-linux/amd64}"
LOCAL_IMAGE="llm-api:${SHA}${DIRTY}"
TAGS_LOCAL=("$LOCAL_IMAGE" "llm-api:latest")

ssh_opts=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)
if [ -n "$SERVER_PORT" ] && [ "$SERVER_PORT" != "22" ]; then
  ssh_opts+=(-p "$SERVER_PORT")
fi

# ---------- SSH 执行函数 ----------
run_ssh() {
  local remote_cmd="$1"

  if [ -z "$SERVER_SSH" ]; then
    echo "[错误] SERVER_SSH 未配置"
    return 1
  fi

  echo "==> SSH → ${SERVER_SSH}:${SERVER_PORT:-22} (${SERVER_DIR})"
  if [ -n "$SERVER_PASS" ]; then
    command -v sshpass >/dev/null 2>&1 || {
      echo "[错误] 设置了 SERVER_PASS 但未安装 sshpass"
      return 1
    }
    SSHPASS="$SERVER_PASS" sshpass -e ssh "${ssh_opts[@]}" "$SERVER_SSH" "$remote_cmd"
  else
    ssh "${ssh_opts[@]}" "$SERVER_SSH" "$remote_cmd"
  fi
}

# ---------- 服务器部署 ----------
run_deploy() {
  local remote_cmd="cd $(printf '%q' "$SERVER_DIR") && \
    IMAGE_TAG=${SHA}${DIRTY} docker compose up -d --remove-orphans"
  run_ssh "$remote_cmd"
}

# --deploy-only：只部署，不构建、不传输
if [ "$DEPLOY_ONLY" -eq 1 ]; then
  echo "==> [仅部署] 重启容器（IMAGE_TAG=${SHA}${DIRTY}）"
  run_deploy
  echo ""
  echo "部署完成 ✔  IMAGE_TAG=${SHA}${DIRTY}"
  exit 0
fi

# ---------- 1. 构建镜像 ----------
echo "==> [1/3] 构建镜像 ($PLATFORM)，分支 $BRANCH @ $SHA"
BUILD_ARGS=(--platform "$PLATFORM" --progress=plain)
for t in "${TAGS_LOCAL[@]}"; do BUILD_ARGS+=(-t "$t"); done
docker build "${BUILD_ARGS[@]}" .

# ---------- 2. 保存并传输镜像到服务器 ----------
echo "==> [2/3] 保存镜像并通过 SSH 传输到服务器（docker save | ssh docker load）"
if [ -n "$SERVER_PASS" ]; then
  command -v sshpass >/dev/null 2>&1 || {
    echo "[错误] 设置了 SERVER_PASS 但未安装 sshpass"
    exit 1
  }
  SSHPASS="$SERVER_PASS" sshpass -e ssh "${ssh_opts[@]}" "$SERVER_SSH" "mkdir -p $(printf '%q' "$SERVER_DIR")"
  docker save "$LOCAL_IMAGE" | SSHPASS="$SERVER_PASS" sshpass -e ssh "${ssh_opts[@]}" "$SERVER_SSH" "docker load"
else
  ssh "${ssh_opts[@]}" "$SERVER_SSH" "mkdir -p $(printf '%q' "$SERVER_DIR")"
  docker save "$LOCAL_IMAGE" | ssh "${ssh_opts[@]}" "$SERVER_SSH" "docker load"
fi

# ---------- 3. 服务器部署 ----------
if [ "$NO_DEPLOY" -eq 1 ]; then
  echo "==> [3/3] --no-deploy，跳过服务器重启"
  echo ""
  echo "镜像已传输完成 ✔  ${LOCAL_IMAGE}"
  echo "手动部署：ssh ${SERVER_SSH} 'cd ${SERVER_DIR} && IMAGE_TAG=${SHA}${DIRTY} docker compose up -d'"
else
  echo "==> [3/3] 服务器重启容器（IMAGE_TAG=${SHA}${DIRTY}）"
  run_deploy
  echo ""
  echo "发布完成 ✔  本地镜像：${LOCAL_IMAGE}"
  echo "线上 IMAGE_TAG：${SHA}${DIRTY}"
  echo "回滚：ssh ${SERVER_SSH} 'cd ${SERVER_DIR} && IMAGE_TAG=<短SHA> docker compose up -d'"
fi
