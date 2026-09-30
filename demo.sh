#!/usr/bin/env bash
# 端到端演示：预演、相关配置整体生效、部分失败、补偿、离线连改、重启续作。
set -euo pipefail

cd "$(dirname "$0")"
DEMO_DIR="./data/demo"
rm -rf "$DEMO_DIR"
mkdir -p "$DEMO_DIR"
ADDR="127.0.0.1:18090"
BASE="http://$ADDR"

go build -o "$DEMO_DIR/twinserver" ./cmd/twinserver
go build -o "$DEMO_DIR/virtualdevice" ./cmd/virtualdevice

"$DEMO_DIR/twinserver" -addr "$ADDR" -db "$DEMO_DIR/state.json" &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT

dev() { "$DEMO_DIR/virtualdevice" -server "$BASE" -id "$1" -state "$DEMO_DIR/$1.json" "${@:2}"; }
sleep 0.5

echo '== 1) 为两台设备建立已确认基线 =='
curl -s -X PUT "$BASE/devices/dev-a/desired" -H 'Content-Type: application/json' -d '{"temp":"20","mode":"eco"}'; echo
curl -s -X PUT "$BASE/devices/dev-b/desired" -H 'Content-Type: application/json' -d '{"temp":"20","mode":"eco"}'; echo
dev dev-a poll; dev dev-a apply
dev dev-b poll; dev dev-b apply

echo '== 2) 新目标发布前只预演差异，不产生版本 =='
curl -s -X POST "$BASE/changes/preview" -H 'Content-Type: application/json' \
  -d '{"targets":{"dev-a":{"temp":"24","mode":"turbo"},"dev-b":{"temp":"24","mode":"turbo"}}}'; echo

echo '== 3) 发布相关配置组：一台接受、一台拒绝，组仍是 PARTIAL =='
GROUP=$(curl -s -X POST "$BASE/changes" -H 'Content-Type: application/json' \
  -d '{"targets":{"dev-a":{"temp":"24","mode":"turbo"},"dev-b":{"temp":"24","mode":"turbo"}}}')
echo "$GROUP"
GROUP_ID=$(printf '%s' "$GROUP" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
dev dev-a poll; dev dev-a apply
dev dev-b poll; dev dev-b apply -reject mode
curl -s "$BASE/changes/$GROUP_ID"; echo

echo '== 4) 失败后发布补偿目标；补偿只表示已下发，必须重新设备确认 =='
COMP=$(curl -s -X POST "$BASE/changes/$GROUP_ID/compensate" -H 'Content-Type: application/json' -d '{}')
echo "$COMP"
COMP_ID=$(printf '%s' "$COMP" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
dev dev-a poll; dev dev-b poll
curl -s "$BASE/changes/$COMP_ID"; echo
dev dev-a resume; dev dev-b resume
curl -s "$BASE/changes/$COMP_ID"; echo

echo '== 5) 离线期间多次修改；恢复后只拉最新，旧变更保留为 SUPERSEDED 历史 =='
dev dev-a offline
curl -s -X PUT "$BASE/devices/dev-a/desired" -H 'Content-Type: application/json' -d '{"temp":"30"}'; echo
curl -s -X PUT "$BASE/devices/dev-a/desired" -H 'Content-Type: application/json' -d '{"temp":"32"}'; echo
dev dev-a online; dev dev-a poll; dev dev-a resume
dev dev-a late-report
curl -s "$BASE/changes"; echo

echo '== 6) 服务重启后，设备从本地缓存继续未完成确认 =='
curl -s -X PUT "$BASE/devices/dev-c/desired" -H 'Content-Type: application/json' -d '{"temp":"18"}'; echo
dev dev-c poll
kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true
"$DEMO_DIR/twinserver" -addr "$ADDR" -db "$DEMO_DIR/state.json" &
SERVER_PID=$!
sleep 0.5
dev dev-c resume
curl -s "$BASE/devices/dev-c/twin"; echo

echo; echo '演示完成'
