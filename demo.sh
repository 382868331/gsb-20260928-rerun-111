#!/usr/bin/env bash
# 端到端演示：服务 + 本地虚拟设备，覆盖部分拒绝、迟到上报、离线连改、重启保留。
# 用法: ./demo.sh
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

dev() { "$DEMO_DIR/virtualdevice" -server "$BASE" -id dev-1 -state "$DEMO_DIR/dev-1.json" "$@"; }
api() { echo; echo "### $1"; shift; "$@"; echo; }
sleep 0.5

echo '== 1) 下发 rev1，设备拉取后只显示“已下发”，确认后才“已生效” =='
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type: application/json' -d '{"temp":"22","mode":"eco"}'; echo
dev poll
curl -s "$BASE/devices/dev-1/twin"; echo
dev apply
curl -s "$BASE/devices/dev-1/twin"; echo

echo '== 2) rev2：设备拒绝 fan 配置，状态为部分生效，冲突可查询 =='
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type: application/json' -d '{"temp":"22","mode":"eco","fan":"high"}'; echo
dev poll
dev apply -reject fan
curl -s "$BASE/devices/dev-1/conflicts"; echo

echo '== 3) rev3 改为设备支持的配置，全部生效，旧拒绝冲突关闭 =='
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type: application/json' -d '{"temp":"23","mode":"eco"}'; echo
dev poll
dev apply

echo '== 4) rev4 后模拟迟到的 rev3 上报：不能盖掉最新目标 =='
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type:application/json' -d '{"temp":"28"}'; echo
dev poll
dev apply
dev late-report
curl -s "$BASE/devices/dev-1/twin"; echo

echo '== 5) 离线期间连续修改 rev6、rev7，恢复后只执行最新版本 =='
dev offline
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type: application/json' -d '{"temp":"30"}'; echo
curl -s -X PUT "$BASE/devices/dev-1/desired" -H 'Content-Type: application/json' -d '{"temp":"32"}'; echo
dev poll
dev apply
curl -s "$BASE/devices/dev-1/conflicts?all=true"; echo

echo '== 6) 重启服务：未确认变更在重启后保留 =='
curl -s -X PUT "$BASE/devices/dev-2/desired" -H 'Content-Type: application/json' -d '{"temp":"19"}'; echo
dev2() { "$DEMO_DIR/virtualdevice" -server "$BASE" -id dev-2 -state "$DEMO_DIR/dev-2.json" "$@"; }
dev2 poll
kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true
"$DEMO_DIR/twinserver" -addr "$ADDR" -db "$DEMO_DIR/state.json" &
SERVER_PID=$!
sleep 0.5
curl -s "$BASE/devices/dev-2/twin"; echo
dev2 apply
curl -s "$BASE/devices/dev-2/twin"; echo

echo '== 7) 预演差异：不发布、不推进版本 =='
curl -s -X POST "$BASE/devices/dev-2/preview" -H 'Content-Type: application/json' -d '{"temp":"16","mode":"eco"}'; echo
curl -s "$BASE/devices/dev-2/changes"; echo

echo '== 8) 多设备相关配置整体生效：dev-3 成功不等于 dev-4 生效 =='
T8=$(curl -s -X POST "$BASE/targets" -H 'Content-Type: application/json" \
  -d '{"devices":{"dev-3":{"temp":"21"},"dev-4":{"temp":"27"}}}' | sed -n 's/.*"id": *"\(T[0-9]*\)".*/\1/p')
dev3() { "$DEMO_DIR/virtualdevice" -server "$BASE" -id dev-3 -state "$DEMO_DIR/dev-3.json" "$@"; }
dev4() { "$DEMO_DIR/virtualdevice" -server "$BASE" -id dev-4 -state "$DEMO_DIR/dev-4.json" "$@"; }
dev3 poll; dev3 apply
curl -s "$BASE/targets/$T8"; echo
dev4 poll; dev4 apply
curl -s "$BASE/targets/$T8"; echo

echo '== 9) 失败后补偿目标：默认回退到已确认状态，补偿也需设备确认 =='
TID=$(curl -s -X POST "$BASE/targets" -H 'Content-Type: application/json' \
  -d '{"devices":{"dev-3":{"temp":"40","fan":"max"}}}' | sed -n 's/.*"id": *"\(T[0-9]*\)".*/\1/p')
dev3 poll; dev3 apply -reject fan
curl -s "$BASE/targets/$TID"; echo
CID=$(curl -s -X POST "$BASE/targets/$TID/compensate" -H 'Content-Type: application/json' -d '{}' | sed -n 's/.*"id": *"\(T[0-9]*\)".*/\1/p')
curl -s "$BASE/targets/$CID"; echo
dev3 poll; dev3 apply
curl -s "$BASE/targets/$CID"; echo

echo '== 10) 设备重启：从本地缓存继续未完成操作，被替代版本记录仍保留 =='
dev4 restart
curl -s "$BASE/devices/dev-4/changes"; echo

echo; echo '演示完成'
