# 设备状态孪生服务（v1）

用 Go 标准库实现的第一版设备状态孪生服务。后续更多配置下发都沿用本版的版本化期望状态关系。

## 三种状态

每个设备的每个配置项都分开保存：

- desired（期望状态）：平台希望设备达到的值，带单调递增版本号和下发状态。
- reported（设备上报）：设备上报的原始实际值，不等于已确认生效。
- confirmed（实际已确认状态）：设备带着匹配期望版本号的确认上报后才写入。

期望项状态机：

```
pending（待下发） -> delivered（已下发未确认） -> effective（已生效，版本匹配的确认）
                                                    rejected（设备拒绝，记冲突）
```

关键语义：已下发（delivered）不等于已生效（effective）；不带版本号的普通上报只进 reported，不会确认任何目标。

## 关键行为

- 离线连续修改不积压过时要求：设备离线期间同一配置项改多次，版本递增但只保留最新目标，设备恢复后只拉最新一版。
- 旧确认不能盖掉最新目标：上报版本号小于当前期望版本时记为 stale_ack 冲突，目标状态不变、不写 confirmed，设备仍会拉到最新版本。
- 部分配置拒绝：设备可只接受部分 key，被拒 key 状态为 rejected 并记 rejected 冲突，其余 key 正常生效。
- 迟到上报：旧版本确认先到不污染状态，正确版本确认后到才生效。
- 冲突可查询：GET /api/devices/{id}/conflicts，类型为 stale_ack / rejected / diverged。
- 重启保留未完成变更：全部状态（pending/delivered、冲突记录、confirmed）以 JSON 原子写入本地文件（临时文件加 rename），重启后恢复。

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/devices | 注册设备，body: {"id":"vdev-1"} |
| GET | /api/devices | 列出全部设备孪生 |
| GET | /api/devices/{id} | 查设备三态 |
| PUT | /api/devices/{id}/desired | 设期望配置，body: {"config":{"mode":"C"}} |
| POST | /api/devices/{id}/poll | 设备轮询，返回全部待下发项；拉取即置 delivered |
| POST | /api/devices/{id}/report | 设备上报（带版本确认/拒绝，或无版本快照） |
| GET | /api/devices/{id}/conflicts | 查询冲突记录 |

report 的 body 两种形式可同时使用：

```json
{
  "items": [
    {"key":"mode","value":"C","version":3,"applied":true},
    {"key":"interval","value":"10","version":1,"applied":false,"reason":"out of range"}
  ],
  "snapshot": {"firmware": "1.2.3"}
}
```

## 本地运行

```bash
go run ./cmd/twinsvc -addr :8080 -db data/twin.json

go run ./cmd/vdevice -server http://127.0.0.1:8080 -id vdev-1 \
  -interval 1s -db data/vdev.json -reject interval -stale mode

curl -s -X PUT localhost:8080/api/devices/vdev-1/desired \
  -d '{"config":{"mode":"A","interval":"5"}}'
curl -s -X PUT localhost:8080/api/devices/vdev-1/desired -d '{"config":{"mode":"B"}}'
curl -s -X PUT localhost:8080/api/devices/vdev-1/desired -d '{"config":{"mode":"C"}}'

curl -s localhost:8080/api/devices/vdev-1 | jq .
curl -s localhost:8080/api/devices/vdev-1/conflicts | jq .
```

虚拟设备参数：

- -reject key 或 -reject key=value：拒绝某 key（任意值）或特定值，可重复。
- -stale key：应用某 key 后先发一个上一版本的迟到确认，再发正确确认，用于测试旧确认不覆盖目标。

也可以直接运行 ./demo.sh 看完整演示。

## 测试

运行 go test ./...。覆盖离线连续修改合并、迟到旧确认、部分拒绝、无版本上报不确认、重启恢复、相同值幂等，以及完整 HTTP 端到端流程。

## 代码结构

```
pkg/twin/        核心：三态模型、版本化期望状态、冲突记录、JSON 持久化、HTTP 接口
cmd/twinsvc/     服务进程
cmd/vdevice/     本地虚拟设备（轮询拉取、应用/拒绝、迟到确认模拟）
```
