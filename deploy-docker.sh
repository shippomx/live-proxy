#!/usr/bin/env bash
# 以容器方式部署 live-proxy 到 OpenWrt/Kwrt 路由器（与 deploy.sh 的 procd 方式二选一）。
#
#   HOST=r5s PORT=9000 RELAY=false ./deploy-docker.sh
#
# 镜像在路由器本地 build（FROM scratch，不拉任何基础镜像），本机不需要 Docker。
# 每次部署打一个时间戳 tag，latest 指向最新一次，旧 tag 保留用于回滚。
#
# 若 PORT 已被 procd 方式的 live-proxy 占用，默认中止；确认要切换时加 REPLACE_NATIVE=1，
# 脚本会停掉并禁用 procd 服务（不删除二进制和 init 脚本，可随时切回）。
set -euo pipefail

HOST="${HOST:-r5s}"
PORT="${PORT:-9000}"
RELAY="${RELAY:-false}"
REPLACE_NATIVE="${REPLACE_NATIVE:-0}"
REMOTE_DIR=/opt/live-proxy
SERVICE=live-proxy
TAG=$(date +%Y%m%d-%H%M%S)

cd "$(dirname "$0")"
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT

echo "==> 交叉编译 linux/arm64"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$ctx/${SERVICE}" .
ls -lh "$ctx/${SERVICE}" | awk '{print "    大小: " $5}'

echo "==> 检查端口 ${PORT} 占用"
# host 网络下容器内进程在 netstat 里同样叫 live-proxy，只能按 cmdline 区分：
# procd 方式是 /usr/bin/live-proxy，容器里是 /live-proxy。
holder=$(ssh "$HOST" "pid=\$(netstat -tlnp 2>/dev/null | awk '\$4 ~ /:${PORT}\$/ {split(\$7,a,\"/\"); print a[1]; exit}'); [ -n \"\$pid\" ] && tr '\\0' ' ' < /proc/\$pid/cmdline" || true)
native_on_port=0
case "$holder" in
  "")                      ;;
  "/live-proxy "*)         echo "    由旧容器占用，重新部署" ;;
  "/usr/bin/${SERVICE} "*) native_on_port=1
                           if [ "$REPLACE_NATIVE" != 1 ]; then
                             echo "    !! 端口 ${PORT} 正被 procd 方式的 ${SERVICE} 占用" >&2
                             echo "    !! 确认切换到容器请加 REPLACE_NATIVE=1；或先换端口并行试运行，如 PORT=9001" >&2
                             exit 1
                           fi ;;
  *)                       echo "    !! 端口 ${PORT} 被其他进程占用: ${holder}，中止" >&2; exit 1 ;;
esac

echo "==> 上传构建上下文到 ${HOST}:${REMOTE_DIR}"
# data 存扫码登录的 Cookie：属主须是容器内的 nobody，且只有它能读。
ssh "$HOST" "mkdir -p ${REMOTE_DIR}/build ${REMOTE_DIR}/data && chown 65534:65534 ${REMOTE_DIR}/data && chmod 700 ${REMOTE_DIR}/data"
scp -O -q "$ctx/${SERVICE}" Dockerfile "$HOST:${REMOTE_DIR}/build/"
scp -O -q docker-compose.yml "$HOST:${REMOTE_DIR}/"
# CA 证书取路由器自己的：scratch 镜像里没有证书，缺了所有 HTTPS 取流都会失败。
ssh "$HOST" "cp /etc/ssl/certs/ca-certificates.crt ${REMOTE_DIR}/build/"

echo "==> 在路由器上构建镜像 ${SERVICE}:${TAG}"
ssh "$HOST" "docker build -q -t ${SERVICE}:${TAG} -t ${SERVICE}:latest ${REMOTE_DIR}/build"

if [ "$native_on_port" = 1 ]; then
  echo "==> 停止并禁用 procd 方式的 ${SERVICE}（文件保留，可切回）"
  ssh "$HOST" "/etc/init.d/${SERVICE} stop; /etc/init.d/${SERVICE} disable"
fi

echo "==> 启动容器"
# 写进 .env：以后在路由器上直接 docker compose up -d 也沿用同样的端口和中转设置。
ssh "$HOST" "cd ${REMOTE_DIR} && printf 'PORT=%s\nRELAY=%s\n' '${PORT}' '${RELAY}' > .env && docker compose up -d --force-recreate"

echo "==> 验证"
for _ in 1 2 3 4 5; do
  sleep 1
  if ssh "$HOST" "curl -sf -m 5 http://127.0.0.1:${PORT}/healthz"; then
    echo ""
    lan_ip=$(ssh "$HOST" "uci get network.lan.ipaddr 2>/dev/null" || true)
    echo "==> 部署成功: http://${lan_ip:-<路由器IP>}:${PORT}/  (镜像 ${SERVICE}:${TAG})"
    echo "    日志:   ssh ${HOST} docker logs -f ${SERVICE}"
    echo "    回滚到上一版镜像: ssh ${HOST} 'docker tag ${SERVICE}:<旧tag> ${SERVICE}:latest && cd ${REMOTE_DIR} && docker compose up -d --force-recreate'"
    exit 0
  fi
done
echo "!! 健康检查失败，查看日志: ssh ${HOST} docker logs ${SERVICE}" >&2
exit 1
