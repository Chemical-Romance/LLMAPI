#!/usr/bin/env bash
#
# 新服务器傻瓜式部署：自带 MongoDB，不依赖 skillbook / nginx。
# 用法（在项目根目录）：./deploy-standalone.sh
#
# 首次运行会自动生成 .env（随机管理员密码），然后构建并启动容器。
# 重复运行 = 更新代码后重新构建并重启，保留数据与管理员密码。
set -euo pipefail
cd "$(dirname "$0")"

COMPOSE_FILE="docker-compose.standalone.yml"
PORT="${PORT:-8081}"

if ! command -v docker >/dev/null 2>&1; then
  echo "[错误] 未安装 Docker。可执行：curl -fsSL https://get.docker.com | sh"
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "[错误] 未安装 docker compose 插件。请安装 docker-compose-plugin 后重试"
  exit 1
fi

gen_pass() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 24 | LC_ALL=C tr -dc 'A-Za-z0-9' | head -c 20
  else
    head -c 256 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | head -c 20
  fi
}

if [ ! -f .env ]; then
  PASS="$(gen_pass || true)"
  if [ "${#PASS}" -lt 12 ]; then
    echo "[错误] 生成管理员密码失败，请手动创建 .env（参考 .env.example）"
    exit 1
  fi
  cat > .env <<EOF
ADMIN_USER=admin
ADMIN_PASSWORD=${PASS}
MONGO_DB=llm_api
FRAME_ANCESTORS=https://saas.snowbeasts.com
EOF
  chmod 600 .env
  echo "==> 已生成 .env（管理员 admin / ${PASS}），请妥善保存"
else
  echo "==> 使用已有 .env"
fi

echo "==> 构建并启动容器（首次构建需要几分钟）"
docker compose -f "$COMPOSE_FILE" up -d --build

echo "==> 等待服务就绪"
ok=0
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${PORT}/console/" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then ok=1; break; fi
  sleep 2
done

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$IP" ] || IP="<服务器IP>"
echo ""
docker compose -f "$COMPOSE_FILE" ps
echo ""
if [ "$ok" -eq 1 ]; then
  echo "部署成功 ✔"
else
  echo "[警告] 健康检查未通过，查看日志：docker compose -f ${COMPOSE_FILE} logs -f llm-router"
fi
echo "控制台：http://${IP}:${PORT}/console/"
echo "管理员：$(grep '^ADMIN_USER=' .env | cut -d= -f2-) / $(grep '^ADMIN_PASSWORD=' .env | cut -d= -f2-)"
