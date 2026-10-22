#!/usr/bin/env bash
#
# LLMAPI 本地预编译发布脚本 —— 在【本地开发机】执行
# 功能：本地交叉编译 linux/amd64 Go 二进制 → 通过 rsync 传输到服务器 → 服务器 docker compose up -d --build 重启容器
# 类似 SkillBook 的产物传输方式，不在服务器编译 Go 代码。
#
# 前置条件：
#   1. 本地已安装 Go（>=1.27）
#   2. 服务器已配置 SSH 密钥可免密登录（推荐），或在下方填入 SERVER_PASS 并安装 sshpass
#   3. 服务器部署目录已存在（脚本会自动创建）
#
# 用法：
#   ./release-artifacts.sh                # 编译 → 传输 → 部署
#   ./release-artifacts.sh --no-deploy     # 只编译并传输，不重启容器
#   ./release-artifacts.sh --deploy-only  # 跳过编译与传输，只连服务器重启容器
#
# 回滚：将对应版本的 llm-api 二进制重新传输覆盖，或使用 git 历史找回旧版二进制再发布。

set -euo pipefail
cd "$(dirname "$0")"

# ============================================================
# 服务器部署配置（仅本机使用，可考虑加入 .gitignore）
#   SERVER_SSH   目标主机，例：root@203.0.113.10 或 ssh 别名
#   SERVER_DIR   服务器上的部署目录，例：/usr/local/code/llm-api
#   SERVER_PORT  SSH 端口（默认 22）
#   SERVER_PASS  密码登录时填写（需安装 sshpass）；留空 = 密钥认证
# 以上配置优先从环境变量读取，也可放在项目根目录的 .release.env
# （该文件已被 .gitignore 忽略，避免把服务器密码提交到仓库）。
# ============================================================
if [ -f .release.env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.release.env
  set +a
fi
SERVER_SSH="${SERVER_SSH:-}"
SERVER_DIR="${SERVER_DIR:-/usr/local/code/llm-api}"
SERVER_PORT="${SERVER_PORT:-22}"
SERVER_PASS="${SERVER_PASS:-}"

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

# ---------- 前置检查 ----------
command -v go >/dev/null 2>&1 || { echo "[错误] 本地未安装 go"; exit 1; }
command -v ssh >/dev/null 2>&1 || { echo "[错误] 未找到 ssh"; exit 1; }
command -v rsync >/dev/null 2>&1 || { echo "[错误] 未找到 rsync（推荐用于传输产物）"; exit 1; }

BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || echo unknown)"
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"

# 服务器目标架构（默认 linux/amd64）
export GOOS="${GOOS:-linux}"
export GOARCH="${GOARCH:-amd64}"
CGO_ENABLED=0

ssh_opts=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)
if [ -n "$SERVER_PORT" ] && [ "$SERVER_PORT" != "22" ]; then
  ssh_opts+=(-p "$SERVER_PORT")
fi

# ---------- SSH 执行函数 ----------
run_ssh() {
  local remote_cmd="$1"

  if [ -z "$SERVER_SSH" ]; then
    echo "[错误] SERVER_SSH 未配置（请先编辑 release.sh 填写服务器信息）"
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
  local remote_cmd="cd $(printf '%q' "$SERVER_DIR") && docker compose up -d --build --remove-orphans"
  run_ssh "$remote_cmd"
}

# --deploy-only
if [ "$DEPLOY_ONLY" -eq 1 ]; then
  echo "==> [仅部署] 跳过编译与传输，直接重启容器"
  run_deploy
  echo ""
  echo "部署完成 ✔"
  exit 0
fi

# ---------- 1. 本地编译 ----------
echo "==> [1/3] 本地交叉编译（GOOS=${GOOS} GOARCH=${GOARCH} CGO_ENABLED=0），分支 ${BRANCH} @ ${SHA}"
mkdir -p ./dist
go build -trimpath -ldflags="-s -w" -o ./dist/llm-api ./cmd/server

local_size="$(ls -lh ./dist/llm-api | awk '{print $5}')"
echo "    编译完成：./dist/llm-api（${local_size}）"

# ---------- 2. 传输产物到服务器 ----------
if [ -z "$SERVER_SSH" ]; then
  echo "[错误] SERVER_SSH 未配置，请先编辑 $(basename "$0") 填写服务器连接信息"
  exit 1
fi

echo "==> [2/3] 传输产物到服务器（rsync）"
# 确保服务器目录存在
run_ssh "mkdir -p $(printf '%q' "$SERVER_DIR")" >/dev/null

# 只传需要的文件：编译好的二进制 + 预编译用 Dockerfile + compose
# rsync 复用同一套 ssh 选项；密码模式下通过 sshpass 包装
RSYNC_RSH="ssh ${ssh_opts[*]}"
if [ -n "$SERVER_PASS" ]; then
  command -v sshpass >/dev/null 2>&1 || {
    echo "[错误] 设置了 SERVER_PASS 但未安装 sshpass"
    exit 1
  }
  export SSHPASS="$SERVER_PASS"
  RSYNC_RSH="sshpass -e ssh ${ssh_opts[*]}"
fi

set +e
rsync -avz --progress -e "$RSYNC_RSH" \
  ./dist/llm-api \
  ./Dockerfile.prebuilt \
  ./docker-compose.yml \
  "${SERVER_SSH}:${SERVER_DIR}/"
rsync_rc=$?
set -e
if [ "$rsync_rc" -ne 0 ]; then
  echo "[错误] rsync 传输失败（退出码 ${rsync_rc}；23=部分文件失败，24=源文件消失，255=SSH 连接错误）"
  exit "$rsync_rc"
fi

# ---------- 3. 服务器部署 ----------
if [ "$NO_DEPLOY" -eq 1 ]; then
  echo "==> [3/3] --no-deploy，跳过服务器重启"
  echo ""
  echo "产物已传输完成 ✔  ./dist/llm-api → ${SERVER_SSH}:${SERVER_DIR}/llm-api"
  echo "手动部署：ssh ${SERVER_SSH} 'cd ${SERVER_DIR} && docker compose up -d --build'"
else
  echo "==> [3/3] 服务器构建镜像并重启容器"
  run_deploy
  echo ""
  echo "发布完成 ✔  分支 ${BRANCH} @ ${SHA}"
  echo "线上已使用本地预编译二进制部署"
fi
