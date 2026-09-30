# 设备状态孪生服务（Go 第一版）

一个从零实现的设备状态孪生（device twin）服务。核心是把三类状态严格分开：

- **期望状态 desired**：服务端最新配置目标，带单调递增的 `desired_rev`。以后新增的配置下发都挂在这个版本关系上。
- **设备上报 reported**：设备最近一次上报的运行配置和它所针对的版本，仅供观察。
- **实际已确认状态 confirmed**：服务端核对上报与期望一致后才推进；确认只前进、不回退。

因此“已下发”（`DELIVERED`）绝不会显示成“已生效”（`EFFECTIVE`）。

## 状态机

| 状态 | 含义 |
| --- | --- |
| `PENDING` 待下发 | 有新期望，设备还没拉取 |
| `DELIVERED` 已下发 | 设备已拉取，但尚未确认生效 |
| `PARTIAL` 部分生效 | 设备拒绝了部分配置项 |
| `EFFECTIVE` 已生效 | 设备按当前期望版本全部确认 |

## 关键一致性规则

- **离线连续修改**：设备离线时的多次期望修改只推进版本号；设备恢复拉取时只拿最新版本，不会顺序补执行过时要求。一个已下发但未确认、随后被取代的版本会记录 `SUPERSEDED` 冲突。
- **迟到上报**：上报版本低于当前 `desired_rev` 时标记 `stale=true`，只更新 reported 观察值并记录 `STALE_REPORT` 冲突；不改动期望、不推进/回退确认状态。
- **部分拒绝**：上报里带 `rejected` 键值时状态为 `PARTIAL` 并记录 `REJECTED` 冲突；后续期望被完全接受后冲突自动关闭。
- **重启保留**：所有状态原子写入 JSON 文件（临时文件 + rename），服务重启后未完成的变更仍是 `PENDING`/`DELIVERED`，设备可继续完成。
- **冲突可查询**：`GET /devices/{id}/conflicts` 只看未解决冲突，`?all=true` 包含历史。

## 第二轮：整体生效、预演与补偿

- **一组配置整体生效**：`POST /targets` 可把相关配置同时发布给多台设备。整体状态由每台设备**各自**的孪生状态聚合，取最未完成的那个：任何一台仍是 `PENDING`/`DELIVERED`/`PARTIAL`，目标都不是 `EFFECTIVE`。一台设备的成功或已下发记录绝不会算到另一台头上。
- **只接受一部分**：设备部分拒绝时，已确认状态只保存它实际接受的键值，目标停在 `PARTIAL`；其余设备不受影响。
- **发布前预演**：`POST /devices/{id}/preview` 返回相对当前期望的 `add` / `remove` / `change` 差异，不发布、不推进版本。
- **失败补偿**：目标未整体生效时可用 `POST /targets/{id}/compensate` 发起补偿目标。补偿是**新的期望版本**（`kind=COMPENSATION`），必须重新下发并由设备确认，不会直接标记生效；请求体为空时默认回退到各设备最近一次已确认配置。
- **变更记录不丢弃**：每次发布都在 `GET /devices/{id}/changes` 留痕（版本、目标、类型、状态）。离线期间被跳过或从未下发完成的版本标记为 `SUPERSEDED` 并保留；迟到的旧版本确认不能推进当前生效版本；服务或设备重新打开后继续未完成操作。

## HTTP 接口

| 方法与路径 | 说明 |
| --- | --- |
| `GET /devices` | 所有设备孪生视图 |
| `GET /devices/{id}/twin` | 单设备三态视图与中文状态 |
| `PUT /devices/{id}/desired` | 下发整份期望配置（JSON 对象），返回新版本号 |
| `POST /devices/{id}/pull` | 设备拉取期望；有新版本时返回 `changed=true`，只标记已下发 |
| `POST /devices/{id}/report` | 设备上报 `{rev, applied, rejected}`；迟到上报返回 `stale=true` |
| `POST /devices/{id}/offline` | 标记设备离线（用于模拟） |
| `GET /devices/{id}/conflicts[?all=true]` | 查询冲突 |
| `POST /devices/{id}/preview` | 预演一份新配置的差异，不改变状态 |
| `GET /devices/{id}/changes` | 该设备全部发布记录（含已被替代的版本） |
| `POST /targets` | 发布多设备目标 `{"devices":{"dev-1":{...}}}` |
| `GET /targets` / `GET /targets/{id}` | 目标列表 / 目标整体与各设备生效情况 |
| `POST /targets/{id}/compensate` | 对未生效目标发起补偿目标（需设备重新确认） |

配置值用字符串键值表示，例如 `{"temp":"22","mode":"eco"}`。

## 运行

```bash
go run ./cmd/twinserver -addr :8080 -db data/twin.json

# 另一个终端：本地虚拟设备
go run ./cmd/virtualdevice -id dev-1 poll        # 拉取最新期望
go run ./cmd/virtualdevice -id dev-1 apply       # 应用并上报全部生效
go run ./cmd/virtualdevice -id dev-1 apply -reject fan,temp   # 模拟拒绝部分配置
go run ./cmd/virtualdevice -id dev-1 late-report # 模拟迟到的旧版本上报
go run ./cmd/virtualdevice -id dev-1 restart     # 模拟设备重启，从本地缓存继续未完成操作
go run ./cmd/virtualdevice -id dev-1 offline     # 模拟离线
go run ./cmd/virtualdevice -id dev-1 state       # 查看设备本地缓存
```

一键演示（需要本机允许监听端口）：

```bash
./demo.sh
```

## 测试

`tests/integration_test.go` 直接驱动 HTTP handler，覆盖：待下发→已下发→已生效、部分拒绝与冲突关闭、离线连续修改只执行最新版、迟到上报不覆盖最新目标、服务重启后未完成变更保留，以及预演不落库、多设备目标整体生效、补偿目标需设备确认、离线连改+旧确认迟到+重启后被替代版本记录保留。

```bash
go test ./...
```

## 代码结构

- `internal/twin`：领域模型（三态）、带锁存储与 JSON 持久化、冲突规则
- `internal/server`：HTTP 接口与状态文案
- `cmd/twinserver`：服务进程
- `cmd/virtualdevice`：本地虚拟设备
- `tests`：HTTP 级集成测试
