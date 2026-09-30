# 设备状态孪生服务（Go 第一版）

一个从零实现的设备状态孪生（device twin）服务。核心是把期望、观察、确认三类状态严格分开，并把一组相关配置建模为需要所有目标设备分别确认的变更：

- **期望状态 desired**：服务端为每台设备维护的最新配置目标，带该设备单调递增的 `desired_rev`，并关联到一个变更组 `change_id`。
- **设备上报 reported**：设备最近一次上报的运行配置和它所针对的版本，仅供观察。
- **实际已确认状态 confirmed**：服务端核对上报与期望一致后才推进；确认只前进、不回退。

因此“已下发”（`DELIVERED`）绝不会显示成“已生效”（`EFFECTIVE`）；一个设备成功确认也不能让同组其他设备或整个变更组生效。变更组只有所有目标设备都对自己的目标精确上报后才是 `EFFECTIVE`。

## 状态机

| 状态 | 含义 |
| --- | --- |
| `PENDING` 待下发 | 有新期望，设备还没拉取 |
| `DELIVERED` 已下发 | 设备已拉取，但尚未确认生效 |
| `PARTIAL` 部分生效 | 设备拒绝了部分配置项 |
| `EFFECTIVE` 已生效 | 设备按当前期望版本全部确认 |
| `FAILED` 未生效 | 设备上报与期望不一致，等待新目标或补偿目标 |
| `SUPERSEDED` 已替代 | 旧目标被新目标替代，只作为历史保留 |

## 关键一致性规则

- **离线连续修改**：设备离线时的多次期望修改只推进版本号；设备恢复拉取时只拿最新版本，不会顺序补执行过时要求。一个已下发但未确认、随后被取代的版本会记录 `SUPERSEDED` 冲突。
- **迟到上报**：上报版本低于当前 `desired_rev` 时标记 `stale=true`，只更新 reported 观察值并记录 `STALE_REPORT` 冲突；不改动期望、不推进/回退确认状态。
- **整体生效条件**：`POST /changes` 一次发布 `{targets:{设备:{配置}}}`；每台设备只确认本设备目标，变更组在所有设备均 `EFFECTIVE` 后才整体生效。
- **部分拒绝/失败**：上报里带 `rejected` 键值时状态为 `PARTIAL` 并记录 `REJECTED` 冲突；非精确且未列拒绝时记录 `APPLIED_MISMATCH`。部分接受不会推进该设备的 `confirmed_rev`。
- **发布前预演**：`POST /changes/preview` 返回每台设备的新增、更新、删除项以及当前期望/观察/确认版本；不创建设备、不推进版本、不落盘。
- **失败补偿**：`POST /changes/{id}/compensate` 创建 `COMPENSATION` 新目标。空目标默认回到原变更发布前每台设备的已确认基线；补偿仍需拉取和设备确认。
- **重启保留**：所有状态原子写入 JSON 文件（临时文件 + rename），服务重启后未完成的变更仍是 `PENDING`/`DELIVERED`，设备可继续完成。
- **替代记录保留**：离线多改或补偿发布后，旧变更标记 `SUPERSEDED`；已经成功的设备结果仍保留在该历史变更中，不会被改写成失败。
- **冲突可查询**：`GET /devices/{id}/conflicts` 只看未解决冲突，`?all=true` 包含历史；被替代记录默认是历史，不占用未解决列表。

## HTTP 接口

| 方法与路径 | 说明 |
| --- | --- |
| `GET /devices` | 所有设备孪生视图 |
| `POST /changes/preview` | 发布前预演差异，不修改状态 |
| `POST /changes` | 发布一组相关配置 |
| `GET /changes` | 查询变更组历史 |
| `GET /changes/{id}` | 查询一个变更组的逐设备结果 |
| `POST /changes/{id}/compensate` | 基于失败变更发布补偿目标 |
| `GET /devices/{id}/twin` | 单设备三态视图与中文状态 |
| `PUT /devices/{id}/desired` | 下发整份期望配置（JSON 对象），返回新版本号 |
| `POST /devices/{id}/pull` | 设备拉取期望；有新版本时返回 `changed=true`，只标记已下发 |
| `POST /devices/{id}/report` | 设备上报 `{rev, applied, rejected}`；迟到上报返回 `stale=true` |
| `POST /devices/{id}/offline` | 标记设备离线（用于模拟） |
| `GET /devices/{id}/conflicts[?all=true]` | 查询冲突 |

配置值用字符串键值表示，例如 `{"temp":"22","mode":"eco"}`。

## 运行

```bash
go run ./cmd/twinserver -addr :8080 -db data/twin.json

# 另一个终端：本地虚拟设备
go run ./cmd/virtualdevice -id dev-1 poll        # 拉取最新期望
go run ./cmd/virtualdevice -id dev-1 apply       # 应用并上报全部生效
go run ./cmd/virtualdevice -id dev-1 apply -reject fan,temp   # 模拟拒绝部分配置
go run ./cmd/virtualdevice -id dev-1 late-report # 模拟迟到的旧版本上报
go run ./cmd/virtualdevice -id dev-1 offline     # 模拟离线
go run ./cmd/virtualdevice -id dev-1 resume      # 模拟设备重启后从本地缓存续作
go run ./cmd/virtualdevice -id dev-1 state       # 查看设备本地缓存
```

一键演示（需要本机允许监听端口）：

```bash
./demo.sh
```

## 测试

`tests/integration_test.go` 直接驱动 HTTP handler，覆盖：待下发→已下发→已生效、部分拒绝与冲突关闭、跨设备整体生效条件、预演不落盘、补偿仍需确认、离线连续修改只执行最新版、迟到上报不覆盖最新目标、服务/设备重开后续作、被替代变更历史保留。

```bash
go test ./...
```

## 代码结构

- `internal/twin`：领域模型（三态）、带锁存储与 JSON 持久化、冲突规则
- `internal/server`：HTTP 接口与状态文案
- `cmd/twinserver`：服务进程
- `cmd/virtualdevice`：本地虚拟设备
- `tests`：HTTP 级集成测试
