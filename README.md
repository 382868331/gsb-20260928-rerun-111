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
| `QUEUED` 等待中 | 目标尚未为该设备生成期望版本，正在等待同一设备先前操作结束 |

## 关键一致性规则

- **变更排队/显式取代**：`POST /changes` 默认使用 `QUEUE`：某设备已有尚未反馈的 `PENDING`/`DELIVERED` 操作时，新目标在该设备维度进入 `QUEUED`，不会覆盖当前目标；其他无冲突设备仍立即继续。请求 `"strategy":"SUPERSEDE"` 才显式取代未完成操作。`PARTIAL`/`FAILED` 已经是该目标的终态反馈，后续排队目标可激活。
- **可继续/需等待**：变更响应中的 `ready_devices` 是已可拉取的设备，`waiting_devices` 是仍被先前操作阻塞的设备；逐设备结果含 `wait_for_change`。
- **离线连续修改**：设备离线时尚未拉取的目标也会阻塞后续排队目标；设备恢复后先完成先前操作，再自动激活下一项。旧的单设备 `PUT /desired` 接口保持显式取代语义。
- **迟到上报**：上报版本低于当前 `desired_rev` 时标记 `stale=true`，只更新 reported 观察值并记录 `STALE_REPORT` 冲突；不改动期望、不推进/回退确认状态。
- **整体生效条件**：`POST /changes` 一次发布 `{targets:{设备:{配置}}}`；每台设备只确认本设备目标，变更组在所有设备均 `EFFECTIVE` 后才整体生效。
- **部分拒绝/失败**：上报里带 `rejected` 键值时状态为 `PARTIAL` 并记录 `REJECTED` 冲突；非精确且未列拒绝时记录 `APPLIED_MISMATCH`。部分接受不会推进该设备的 `confirmed_rev`。
- **发布前预演**：`POST /changes/preview` 返回每台设备的新增、更新、删除项以及当前期望/观察/确认版本；不创建设备、不推进版本、不落盘。
- **失败补偿**：`POST /changes/{id}/compensate` 创建 `COMPENSATION` 新目标。空目标默认回到原变更发布前每台设备的已确认基线；补偿仍需拉取和设备确认。
- **重启保留**：所有状态原子写入 JSON 文件（临时文件 + rename），服务重启后未完成的变更仍是 `PENDING`/`DELIVERED`，设备可继续完成。
- **替代记录保留**：显式取代或补偿发布后，旧变更只在涉及的设备维度标记；已成功、部分拒绝、失败等真实反馈继续保留在历史变更中。整组只有所有必要目标均收到精确反馈才是 `EFFECTIVE`。
- **冲突可查询**：`GET /devices/{id}/conflicts` 只看未解决冲突，`?all=true` 包含历史；被替代记录默认是历史，不占用未解决列表。

## 预约变更（带有效期）

`POST /reservations` 以 `{"targets":{...},"start_at":...,"end_at":...}` 创建预约：到点自动发布生效变更（`RESERVATION`），到期自动安排恢复变更（`RESTORE`）。核心规则：

- **时间到 ≠ 设备完成**：每台设备严格区分 `DUE`（应当生效，等待设备确认）、`CONFIRMED`（已确认生效）、`RESTORE_DUE`（已到期，等待设备核实恢复）、`RESTORED`（已恢复确认）；设备离线时到点/到期照常推进预约，但设备维度停留在等待态并说明原因（如“设备离线，等待重连后拉取预约目标”）。
- **重叠预约**：多台预约可重叠，设备配置按层叠放；内层预约到期后恢复到当时仍然有效的目标（如外层预约或最新常规变更），而不是创建预约时的旧值。
- **缩短与取消**：`PATCH /reservations/{id}` 缩短有效期（填过去的时间等价于立即到期）；`POST /reservations/{id}/cancel` 取消部分设备——未生效的直接取消，已生效的保留当前效果并安排恢复收尾。
- **迟到确认归属**：设备迟到上报按版本号归属到对应预约的生效/恢复变更，不会冲掉在途收尾；重复上报不造成回退。
- **重启续作**：预约、配置层与变更一样原子落盘，服务重开后继续说明每台设备为何等待。

## HTTP 接口

| 方法与路径 | 说明 |
| --- | --- |
| `GET /devices` | 所有设备孪生视图 |
| `POST /changes/preview` | 发布前预演差异，不修改状态 |
| `POST /changes` | 发布一组相关配置，默认排队；可用 `strategy` 选择 `QUEUE`/`SUPERSEDE` |
| `GET /changes` | 查询变更组历史 |
| `GET /changes/{id}` | 查询一个变更组的逐设备结果 |
| `POST /changes/{id}/compensate` | 基于失败变更发布补偿目标 |
| `POST /reservations` | 创建带 `start_at`/`end_at` 有效期的预约变更 |
| `GET /reservations` / `GET /reservations/{id}` | 查询预约整体状态与逐设备进度、等待原因 |
| `PATCH /reservations/{id}` | 缩短有效期（`{"end_at":...}`），可立即到期 |
| `POST /reservations/{id}/cancel` | 取消部分设备（`{"devices":[...]}`），已生效设备安排恢复收尾 |
| `GET /devices/{id}/twin` | 单设备三态视图与中文状态 |
| `PUT /devices/{id}/desired` | 下发整份期望配置（JSON 对象），返回新版本号 |
| `POST /devices/{id}/pull` | 设备拉取期望；有新版本时返回 `changed=true`，只标记已下发 |
| `POST /devices/{id}/report` | 设备上报 `{rev, applied, rejected}`；迟到上报返回 `stale=true` |
| `POST /devices/{id}/offline` | 标记设备离线（用于模拟） |
| `GET /devices/{id}/conflicts[?all=true]` | 查询冲突 |

配置值用字符串键值表示，例如 `{"temp":"22","mode":"eco"}`。默认发布使用 `"strategy":"QUEUE"`；需要立即抢占未完成操作时使用 `"strategy":"SUPERSEDE"`。

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

`tests/integration_test.go` 直接驱动 HTTP handler，覆盖：待下发→已下发→已生效、部分拒绝与冲突关闭、跨设备整体生效条件、预演不落盘、补偿仍需确认、重叠目标排队与显式取代、离线恢复后的队列接续、迟到/重复上报不覆盖最新目标、服务/设备重开后续作、被替代变更历史保留。
`tests/reservation_test.go` 通过虚拟时钟推进，覆盖：两段重叠预约恢复到当时仍有效目标、生效/到期时设备离线后重连续作、迟到确认归属与重复上报、缩短有效期、取消剩余设备并安排收尾、恢复目标参考当前有效操作、状态落盘重开后继续说明等待原因。

```bash
go test ./...
```

## 代码结构

- `internal/twin`：领域模型（三态）、带锁存储与 JSON 持久化、冲突规则
- `internal/server`：HTTP 接口与状态文案
- `cmd/twinserver`：服务进程
- `cmd/virtualdevice`：本地虚拟设备
- `tests`：HTTP 级集成测试
