#!/usr/bin/env bash
set -euo pipefail

# 端到端演示：启动服务和虚拟设备，跑通离线连续修改、部分拒绝、迟到上报，
# 展示三态与冲突后停止进程。

ADDR="127.0.0.1:18090"
BASE="http://${ADDR}"
DIR="$(mktemp -d)"

cleanup() {
  kill "${SVC_PID:-}" "${DEV_PID:-}" 2>/dev/null || true
}
trap cleanup EXIT

echo "state dir: ${DIR}"
go run ./cmd/twinsvc -addr "$ADDR" -db "$DIR/twin.json" &
SVC_PID=$!
sleep 1

curl -s -X POST "$BASE/api/devices" -d '{"id":"vdev-1"}' >/dev/null

go run ./cmd/vdevice -server "$BASE" -id vdev-1 -interval 500ms \
  -db "$DIR/vdev.json" -reject interval -stale mode &
DEV_PID=$!
sleep 1

echo '== 离线式连续修改：A -> B -> C（设备只应执行 C）=='
curl -s -X PUT "$BASE/api/devices/vdev-1/desired" -d '{"config":{"mode":"A","interval":"5"}}' >/dev/null
curl -s -X PUT "$BASE/api/devices/vdev-1/desired" -d '{"config":{"mode":"B"}}' >/dev/null
curl -s -X PUT "$BASE/api/devices/vdev-1/desired" -d '{"config":{"mode":"C"}}' >/dev/null
sleep 3

echo '== 孪生三态 =='
curl -s "$BASE/api/devices/vdev-1" | python3 -m json.tool
echo '== 冲突记录 =='
curl -s "$BASE/api/devices/vdev-1/conflicts" | python3 -m json.tool
