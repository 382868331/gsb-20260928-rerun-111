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
| `SCHEDULED` 预约等待中 | 预约开始时间未到，服务端尚不下发该目标 |
| `EXPIRED` 已到期 | 预约有效期已结束，实际效果仍以设备对恢复目标的确认为准 |
| `CANCELLED` 已取消 | 预约未开始即取消，或执行途中取消该设备的剩余有效期 |

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
- **带有效期预约**：`POST /changes` 可带 `start_at`/`end_at`。未到 `start_at` 是 `SCHEDULED`，时间到了只生成待设备拉取的目标；只有设备精确回报后才是 `EFFECTIVE`。`end_at` 到期不等于设备已恢复，系统会生成 `RESTORATION` 恢复目标并继续等待确认。
- **重叠预约恢复**：多个预约可重叠。新预约生效时只覆盖仍在有效期内的旧预约，不把旧预约永久标记为普通取代；旧预约到期时动态选择当时仍有效的预约目标，没有有效预约时才使用创建预约时记录的已确认基线。
- **执行中调整**：`POST /changes/{id}/shorten` 可按设备缩短有效期，`POST /changes/{id}/cancel` 可取消尚未开始的预约或取消剩余设备；已经确认发生的效果保留在历史中，并自动安排必要恢复收尾。
 - **虚拟时间与等待原因**：`POST /clock`（也兼容 `/clock/advance`、`/time/advance`、`/virtual-time/advance`）推进持久化虚拟时钟。设备离线导致恢复目标停留在 `PENDING`/`DELIVERED` 时，逐设备结果通过 `wait_reason` 说明在等待哪次预约的生效或恢复核实。
- **正常/虚拟时钟一致**：未设置虚拟时钟时，预约的开始与到期同样随真实请求时间推进；两种时钟走同一套到期处理规则，服务重开后也会继续推进重叠预约，恢复目标仍需设备确认。
- **站点级功率约束调度**：`POST /power/reservations` 提交的每项预约声明允许开始窗口（earliest_start/latest_start）、必须连续运行的时长 duration_sec、运行功率 power 与 priority。求解器（回溯搜索，internal/power）选择实际开始时间，使任一时刻站点总功率不超过 `PUT /site/power-limit` 设置的上限；目标先按优先级尽量满足更多预约，再减少启动偏移。无法同时满足时，响应中的 conflict 给出实际冲突预约与超限区间，绝不悄悄缩短任务。
- **功率计划走既有下发/确认流程**：到点的预约被转换成带 start_at/end_at 的预约变更，仍需设备拉取、上报、确认。只发出停止目标而尚未得到设备确认时，原设备功率仍计入站点负载，容量不会提前分给新任务（受影响的预约保持 HELD，不受影响的照常执行）。
- **重排与中断许可**：临时降低站点上限、设备离线、紧急预约（emergency:true）加入都会触发重排，但只重新安排尚未实际开始的普通任务；已经开始的连续运行任务保持不动，只有声明 allow_interrupt 的任务会按许可被中断（记录 INTERRUPTED），无许可任务造成的超限通过 site_overloads 如实报告。正常时钟与虚拟时钟使用同一套规则。

## HTTP 接口

| 方法与路径 | 说明 |
| --- | --- |
| `GET /devices` | 所有设备孪生视图 |
| `POST /changes/preview` | 发布前预演差异，不修改状态 |
| `POST /changes` | 发布一组相关配置，默认排队；可用 `strategy` 选择 `QUEUE`/`SUPERSEDE` |
| `GET /changes` | 查询变更组历史 |
| `GET /changes/{id}` | 查询一个变更组的逐设备结果 |
| `POST /changes/{id}/compensate` | 基于失败变更发布补偿目标 |
| `POST /changes/{id}/shorten` | 缩短一个或多个设备的预约结束时间，body：`{"end_at":...,"devices":[...]}` |
| `POST /changes/{id}/cancel` | 取消预约或取消指定设备的剩余有效期，body：`{"devices":[...]}` |
| `GET /clock` / `POST /clock` | 查询或推进持久化虚拟时间，body：`{"now":"RFC3339"}` |
| `PUT /site/power-limit` | 设置/临时降低站点功率上限，body：`{"limit":10}`；触发重排并返回最新计划 |
| `GET /site/power` | 站点功率视图：上限、当前已承诺负载、逐项预约状态与超限区间 |
| `POST /power/reservations` | 提交功率预约并求解，body：`{"reservations":[{"device_id","config","earliest_start","latest_start","duration_sec","power","priority","allow_interrupt","emergency"}]}` |
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

`tests/integration_test.go` 直接驱动 HTTP handler，覆盖：待下发→已下发→已生效、部分拒绝与冲突关闭、跨设备整体生效条件、预演不落盘、补偿仍需确认、重叠目标排队与显式取代、离线恢复后的队列接续、迟到/重复上报不覆盖最新目标、服务/设备重开后续作、被替代变更历史保留，以及两次重叠预约的虚拟时间推进、离线重连、动态恢复、迟到/重复回报归因、缩短与取消。

```bash
go test ./...
```

## 代码结构

- `internal/twin`：领域模型（三态）、带锁存储与 JSON 持久化、冲突规则
- `internal/server`：HTTP 接口与状态文案
- `cmd/twinserver`：服务进程
- `cmd/virtualdevice`：本地虚拟设备
- `tests`：HTTP 级集成测试
