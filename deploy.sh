#!/usr/bin/env bash
# 部署 live-proxy 到 OpenWrt/Kwrt 路由器。
#
# 目标机磁盘紧张（overlay 分区不足 500MB），所以走静态二进制单文件，
# 不用 Docker —— 镜像层会吃掉好几倍空间，而这个服务本身只有 6MB。
set -euo pipefail

HOST="${HOST:-r5s}"
PORT="${PORT:-9000}"
REMOTE_BIN=/usr/bin/live-proxy
SERVICE=live-proxy

echo "==> 交叉编译 linux/arm64"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/${SERVICE}.bin .
ls -lh /tmp/${SERVICE}.bin | awk '{print "    大小: " $5}'

echo "==> 检查目标机剩余空间"
avail=$(ssh "$HOST" "df -k / | tail -1 | awk '{print \$4}'")
need=$(( $(stat -f %z /tmp/${SERVICE}.bin 2>/dev/null || stat -c %s /tmp/${SERVICE}.bin) / 1024 ))
echo "    需要 ${need}KB，可用 ${avail}KB"
if [ "$avail" -lt $(( need * 3 )) ]; then
  echo "    !! 剩余空间不足需求的 3 倍，中止部署" >&2
  exit 1
fi

echo "==> 停止旧服务（若在运行）"
ssh "$HOST" "[ -x /etc/init.d/${SERVICE} ] && /etc/init.d/${SERVICE} stop 2>/dev/null; killall -9 ${SERVICE} 2>/dev/null; true"

echo "==> 上传二进制"
scp -O /tmp/${SERVICE}.bin "$HOST:${REMOTE_BIN}.new"
ssh "$HOST" "chmod +x ${REMOTE_BIN}.new && mv ${REMOTE_BIN}.new ${REMOTE_BIN}"

echo "==> 安装 procd 服务"
# 定时/常驻任务交给 procd 管理，而不是 nohup 丢后台：
# procd 负责崩溃重启、开机自启和优雅停止，手工起的进程这三样都没有，
# 且进程死掉时不会有任何人知道。
ssh "$HOST" "cat > /etc/init.d/${SERVICE}" <<INIT
#!/bin/sh /etc/rc.common
START=95
STOP=10
USE_PROCD=1

start_service() {
    procd_open_instance
    procd_set_param command ${REMOTE_BIN} -addr :${PORT}
    # OpenWrt 用 /etc/TZ，Go 不认；不显式传 TZ 日志会静默变成 UTC，差 8 小时。
    procd_set_param env TZ=Asia/Shanghai
    procd_set_param respawn 3600 5 0
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}
INIT
ssh "$HOST" "chmod +x /etc/init.d/${SERVICE} && /etc/init.d/${SERVICE} enable && /etc/init.d/${SERVICE} start"

echo "==> 验证"
sleep 2
if ssh "$HOST" "curl -sf -m 5 http://127.0.0.1:${PORT}/healthz" ; then
  echo ""
  lan_ip=$(ssh "$HOST" "uci get network.lan.ipaddr 2>/dev/null" || true)
  echo "==> 部署成功: http://${lan_ip:-<路由器IP>}:${PORT}/"
else
  echo "!! 健康检查失败，查看日志: ssh $HOST logread -e ${SERVICE}" >&2
  exit 1
fi
