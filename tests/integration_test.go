package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"twindemo/internal/server"
	"twindemo/internal/twin"
)

type testClient struct {
	t *testing.T
	h http.Handler
}

func newHarness(t *testing.T) (testClient, *twin.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.json")
	store, err := twin.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return testClient{t: t, h: server.New(store).Routes()}, store, dbPath
}

func newHarnessFromPath(t *testing.T, dbPath string) testClient {
	t.Helper()
	store, err := twin.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return testClient{t: t, h: server.New(store).Routes()}
}

func (c testClient) do(method, path string, body any, out any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s -> %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("解析响应失败: %v: %s", err, raw)
		}
	}
}

func (c testClient) setDesired(id string, cfg twin.Config) int64 {
	var resp struct {
		DesiredRev int64 `json:"desired_rev"`
	}
	c.do(http.MethodPut, "/devices/"+id+"/desired", cfg, &resp)
	return resp.DesiredRev
}

func (c testClient) pull(id string) (int64, twin.Config) {
	var resp struct {
		DesiredRev    int64       `json:"desired_rev"`
		DesiredConfig twin.Config `json:"desired_config"`
	}
	c.do(http.MethodPost, "/devices/"+id+"/pull", nil, &resp)
	return resp.DesiredRev, resp.DesiredConfig
}

func (c testClient) report(id string, req twin.ReportRequest) map[string]any {
	var resp map[string]any
	c.do(http.MethodPost, "/devices/"+id+"/report", req, &resp)
	return resp
}

func (c testClient) twin(id string) twin.TwinView {
	var v twin.TwinView
	c.do(http.MethodGet, "/devices/"+id+"/twin", nil, &v)
	return v
}

func (c testClient) conflicts(id string, all bool) []twin.Conflict {
	q := ""
	if all {
		q = "?all=true"
	}
	var resp struct {
		Conflicts []twin.Conflict `json:"conflicts"`
	}
	c.do(http.MethodGet, "/devices/"+id+"/conflicts"+q, nil, &resp)
	return resp.Conflicts
}

func (c testClient) preview(targets map[string]twin.Config) twin.Preview {
	var p twin.Preview
	c.do(http.MethodPost, "/changes/preview", map[string]any{"targets": targets}, &p)
	return p
}

func (c testClient) publish(targets map[string]twin.Config) twin.Change {
	var ch twin.Change
	c.do(http.MethodPost, "/changes", map[string]any{"targets": targets}, &ch)
	return ch
}

func (c testClient) publishStrategy(targets map[string]twin.Config, strategy twin.ChangeStrategy) twin.Change {
	var ch twin.Change
	c.do(http.MethodPost, "/changes", map[string]any{"targets": targets, "strategy": string(strategy)}, &ch)
	return ch
}

func (c testClient) change(id string) twin.Change {
	var ch twin.Change
	c.do(http.MethodGet, "/changes/"+id, nil, &ch)
	return ch
}

func (c testClient) compensate(id string, targets map[string]twin.Config) twin.Change {
	var ch twin.Change
	body := map[string]any{}
	if targets != nil {
		body["targets"] = targets
	}
	c.do(http.MethodPost, "/changes/"+id+"/compensate", body, &ch)
	return ch
}

func (c testClient) clock(now time.Time) {
	c.do(http.MethodPost, "/clock", map[string]any{"now": now.UTC().Format(time.RFC3339)}, nil)
}

func (c testClient) schedule(targets map[string]twin.Config, start, end time.Time) twin.Change {
	var ch twin.Change
	body := map[string]any{
		"targets":  targets,
		"start_at": start.UTC().Format(time.RFC3339),
		"end_at":   end.UTC().Format(time.RFC3339),
	}
	c.do(http.MethodPost, "/changes", body, &ch)
	return ch
}

func (c testClient) shorten(id string, end time.Time, devices ...string) twin.Change {
	var ch twin.Change
	body := map[string]any{"end_at": end.UTC().Format(time.RFC3339), "devices": devices}
	c.do(http.MethodPost, "/changes/"+id+"/shorten", body, &ch)
	return ch
}

func (c testClient) cancel(id string, devices ...string) twin.Change {
	var ch twin.Change
	body := map[string]any{"devices": devices}
	c.do(http.MethodPost, "/changes/"+id+"/cancel", body, &ch)
	return ch
}

func (c testClient) restoreFor(ch twin.Change, id string) twin.Change {
	c.t.Helper()
	if r := ch.DeviceResults[id]; r.RestoresFor != "" {
		return c.change(r.RestoresFor)
	}
	return c.change(ch.RestoresToChange)
}

// 完整流程：下发 -> 已下发但未生效 -> 确认生效，三态严格区分。
func TestDesiredDeliveredThenEffective(t *testing.T) {
	c, _, _ := newHarness(t)
	rev := c.setDesired("dev-1", twin.Config{"mode": "eco", "temp": "22"})
	if rev != 1 {
		t.Fatalf("首次期望版本应为 1，得到 %d", rev)
	}
	if st := c.twin("dev-1").Status; st != twin.StatusPending {
		t.Fatalf("下发前应为 PENDING，得到 %s", st)
	}

	c.pull("dev-1")
	v := c.twin("dev-1")
	if v.Status != twin.StatusDelivered {
		t.Fatalf("拉取后应为 DELIVERED，得到 %s", v.Status)
	}
	if v.ConfirmedRev != 0 {
		t.Fatalf("已下发不能等同于已生效，confirmed_rev=%d", v.ConfirmedRev)
	}

	c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"mode": "eco", "temp": "22"}})
	v = c.twin("dev-1")
	if v.Status != twin.StatusEffective {
		t.Fatalf("确认后应为 EFFECTIVE，得到 %s", v.Status)
	}
}

// 设备拒绝部分配置：状态为 PARTIAL，冲突可查询，后续接受后恢复 EFFECTIVE。
func TestPartialRejectionConflict(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setDesired("dev-1", twin.Config{"mode": "eco", "temp": "22", "fan": "high"})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{
		Rev:      1,
		Applied:  twin.Config{"mode": "eco", "temp": "22"},
		Rejected: twin.Config{"fan": "high"},
	})
	v := c.twin("dev-1")
	if v.Status != twin.StatusPartial {
		t.Fatalf("部分拒绝后应为 PARTIAL，得到 %s", v.Status)
	}
	open := c.conflicts("dev-1", false)
	if len(open) != 1 || open[0].Reason != twin.ReasonRejected || open[0].Detail["fan"] != "high" {
		t.Fatalf("应记录 1 条 REJECTED 冲突，得到 %+v", open)
	}

	c.setDesired("dev-1", twin.Config{"mode": "eco", "temp": "22"})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 2, Applied: twin.Config{"mode": "eco", "temp": "22"}})
	if st := c.twin("dev-1").Status; st != twin.StatusEffective {
		t.Fatalf("重新全部接受后应为 EFFECTIVE，得到 %s", st)
	}
	if open := c.conflicts("dev-1", false); len(open) != 0 {
		t.Fatalf("冲突解决后不应再有未关闭冲突，得到 %d", len(open))
	}
}

// 离线时连续修改：恢复后只拿到最新版本，旧版本不补执行；被跳过的未完成版本记 SUPERSEDED。
func TestOfflineConsecutiveChanges(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setDesired("dev-1", twin.Config{"temp": "20"}) // rev1
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "20"}})

	c.setDesired("dev-1", twin.Config{"temp": "24"}) // rev2 已下发但未确认
	c.pull("dev-1")
	c.do(http.MethodPost, "/devices/dev-1/offline", nil, nil)

	c.setDesired("dev-1", twin.Config{"temp": "26"}) // rev3 取代未完成的 rev2
	c.setDesired("dev-1", twin.Config{"temp": "28"}) // rev4

	gotRev, gotCfg := c.pull("dev-1") // 设备恢复，只应拿到最新要求
	if gotRev != 4 || gotCfg["temp"] != "28" {
		t.Fatalf("恢复后应只拿到 rev4/temp=28，得到 rev=%d cfg=%v", gotRev, gotCfg)
	}
	c.report("dev-1", twin.ReportRequest{Rev: 4, Applied: twin.Config{"temp": "28"}})
	if st := c.twin("dev-1").Status; st != twin.StatusEffective {
		t.Fatalf("直接按 rev4 生效后应为 EFFECTIVE，得到 %s", st)
	}
	reasons := map[twin.ConflictReason]bool{}
	for _, cf := range c.conflicts("dev-1", true) {
		reasons[cf.Reason] = true
	}
	if !reasons[twin.ReasonSuperseded] {
		t.Fatalf("应记录 rev2 被取代的 SUPERSEDED 冲突，得到 %v", reasons)
	}
}

// 迟到上报：旧版本上报不能盖掉最新目标，也不能回退确认状态，只产生可查询冲突。
func TestLateReportDoesNotOverrideLatest(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setDesired("dev-1", twin.Config{"temp": "22"})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "22"}})

	c.setDesired("dev-1", twin.Config{"temp": "28"}) // rev2
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 2, Applied: twin.Config{"temp": "28"}})

	resp := c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "22"}}) // rev1 迟到
	if stale, _ := resp["stale"].(bool); !stale {
		t.Fatalf("旧版本上报应被标记 stale，得到 %v", resp)
	}
	v := c.twin("dev-1")
	if v.Status != twin.StatusEffective || v.ConfirmedConfig["temp"] != "28" || v.DesiredConfig["temp"] != "28" {
		t.Fatalf("迟到上报不能改动期望或确认状态，得到 %+v", v)
	}
	foundStale := false
	for _, cf := range c.conflicts("dev-1", false) {
		if cf.Reason == twin.ReasonStale {
			foundStale = true
		}
	}
	if !foundStale {
		t.Fatal("迟到上报应产生可查询的 STALE_REPORT 冲突")
	}
}

// 服务重启：从持久化文件重建存储，未完成变更保留为 PENDING / DELIVERED。
func TestRestartPersistsPendingChange(t *testing.T) {
	c, _, dbPath := newHarness(t)
	c.setDesired("dev-1", twin.Config{"temp": "22"}) // 未下发
	c.setDesired("dev-2", twin.Config{"temp": "25"})
	c.pull("dev-2") // 已下发未确认

	store2, err := twin.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c2 := testClient{t: t, h: server.New(store2).Routes()}

	v1 := c2.twin("dev-1")
	v2 := c2.twin("dev-2")
	if v1.DesiredRev != 1 || v1.Status != twin.StatusPending {
		t.Fatalf("重启后 dev-1 未完成变更应保留 PENDING，得到 %+v", v1)
	}
	if v2.Status != twin.StatusDelivered || v2.ConfirmedRev != 0 {
		t.Fatalf("重启后 dev-2 应仍是 DELIVERED 未确认，得到 %+v", v2)
	}

	c2.report("dev-2", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "25"}})
	if st := c2.twin("dev-2").Status; st != twin.StatusEffective {
		t.Fatalf("重启后设备仍可完成未竟变更，得到 %s", st)
	}
}

// 相关配置是一个整体：一台设备成功确认不能让变更组生效，部分设备必须保留实际状态和冲突。
func TestGroupEffectiveRequiresEveryDevice(t *testing.T) {
	c, _, _ := newHarness(t)
	base := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "20"},
		"dev-2": {"temp": "20"},
	})
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: base.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.report("dev-2", twin.ReportRequest{Rev: base.DeviceResults["dev-2"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	if c.change(base.ID).Status != twin.StatusEffective {
		t.Fatalf("两台设备均确认后变更组才应 EFFECTIVE")
	}

	target := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "22"},
		"dev-2": {"temp": "22"},
	})
	rev1 := target.DeviceResults["dev-1"].DesiredRev
	rev2 := target.DeviceResults["dev-2"].DesiredRev
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: rev1, Applied: twin.Config{"temp": "22"}})
	c.report("dev-2", twin.ReportRequest{Rev: rev2, Applied: twin.Config{"temp": "20"}, Rejected: twin.Config{"temp": "22"}})

	ch := c.change(target.ID)
	if ch.Status != twin.StatusPartial {
		t.Fatalf("一台成功、一台拒绝时变更组应为 PARTIAL，得到 %s", ch.Status)
	}
	if ch.DeviceResults["dev-1"].Status != twin.StatusEffective {
		t.Fatalf("应保留 dev-1 自己的成功结果，得到 %+v", ch.DeviceResults["dev-1"])
	}
	if ch.DeviceResults["dev-2"].Status != twin.StatusPartial {
		t.Fatalf("应保留 dev-2 的部分实际状态，得到 %+v", ch.DeviceResults["dev-2"])
	}
	v2 := c.twin("dev-2")
	if v2.Status != twin.StatusPartial || v2.ConfirmedRev != 1 || v2.ConfirmedConfig["temp"] != "20" {
		t.Fatalf("dev-2 不能借用 dev-1 的成功，且确认版本不能推进，得到 %+v", v2)
	}
	if open := c.conflicts("dev-2", false); len(open) != 1 || open[0].Reason != twin.ReasonRejected {
		t.Fatalf("dev-2 应保留未解决拒绝冲突，得到 %+v", open)
	}
}

// 设备针对当前版本上报非期望实际值且未列明拒绝时，状态是失败而非部分生效，并保留冲突。
func TestCurrentVersionMismatchKeepsActualState(t *testing.T) {
	c, _, _ := newHarness(t)
	ch := c.publish(map[string]twin.Config{"dev-1": {"temp": "22", "mode": "eco"}})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: ch.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "21"}})
	v := c.twin("dev-1")
	if v.Status != twin.StatusFailed || v.ConfirmedRev != 0 || v.ReportedConfig["temp"] != "21" {
		t.Fatalf("非期望实际值应保留为观察状态 FAILED 且不能确认，得到 %+v", v)
	}
	open := c.conflicts("dev-1", false)
	if len(open) != 1 || open[0].Reason != twin.ReasonMismatch {
		t.Fatalf("应记录 APPLIED_MISMATCH，得到 %+v", open)
	}
}

// 预演只输出差异，不生成期望版本或变更。
func TestPreviewDoesNotPublish(t *testing.T) {
	c, _, _ := newHarness(t)
	p := c.preview(map[string]twin.Config{"dev-9": {"mode": "eco", "temp": "21"}})
	if len(p.Diffs) != 1 || p.Diffs[0].CurrentRev != 0 || p.Diffs[0].Added["mode"] != "eco" {
		t.Fatalf("预演应给出新增项差异，得到 %+v", p)
	}
	var list struct {
		Devices []twin.TwinView `json:"devices"`
	}
	c.do(http.MethodGet, "/devices", nil, &list)
	if len(list.Devices) != 0 {
		t.Fatalf("预演不能创建设备或发布目标，得到 %+v", list.Devices)
	}
}

// 失败后的补偿是新目标；即使回到旧确认值，也必须重新下发并由每台设备确认。
func TestCompensationRequiresDeviceConfirmation(t *testing.T) {
	c, _, _ := newHarness(t)
	base := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "20"},
		"dev-2": {"temp": "20"},
	})
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: base.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.report("dev-2", twin.ReportRequest{Rev: base.DeviceResults["dev-2"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	failed := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "23"},
		"dev-2": {"temp": "23"},
	})
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: failed.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "23"}})
	c.report("dev-2", twin.ReportRequest{Rev: failed.DeviceResults["dev-2"].DesiredRev, Applied: twin.Config{"temp": "20"}, Rejected: twin.Config{"temp": "23"}})

	comp := c.compensate(failed.ID, nil)
	if comp.Kind != twin.ChangeKindCompensation || comp.CompensatesFor != failed.ID || comp.Status != twin.StatusPending {
		t.Fatalf("补偿应是待设备确认的新目标，得到 %+v", comp)
	}
	if comp.Targets["dev-1"]["temp"] != "20" || comp.Targets["dev-2"]["temp"] != "20" {
		t.Fatalf("未显式指定时补偿目标应回到各自最后确认值，得到 %+v", comp.Targets)
	}
	if c.change(comp.ID).Status != twin.StatusPending {
		t.Fatal("补偿发布不能自动生效")
	}
	c.pull("dev-1")
	c.pull("dev-2")
	if got := c.change(comp.ID).Status; got != twin.StatusDelivered {
		t.Fatalf("补偿仅下发后应为 DELIVERED，得到 %s", got)
	}
	c.report("dev-1", twin.ReportRequest{Rev: comp.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.report("dev-2", twin.ReportRequest{Rev: comp.DeviceResults["dev-2"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	if got := c.change(comp.ID).Status; got != twin.StatusEffective {
		t.Fatalf("补偿经设备确认后才应 EFFECTIVE，得到 %s", got)
	}
	old := c.change(failed.ID)
	if old.Status != twin.StatusSuperseded || old.DeviceResults["dev-1"].Status != twin.StatusEffective || old.DeviceResults["dev-2"].Status != twin.StatusSuperseded {
		t.Fatalf("被补偿替代的失败变更应保留逐设备历史，得到 %+v", old)
	}
}

// 变更组、下发状态和逐设备目标随服务重启恢复，设备重新上线后继续未完成操作。
func TestChangeGroupRestartAndResume(t *testing.T) {
	c, _, dbPath := newHarness(t)
	ch := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "18"},
		"dev-2": {"temp": "19"},
	})
	c.pull("dev-1")

	c2 := newHarnessFromPath(t, dbPath)
	restored := c2.change(ch.ID)
	if restored.Status != twin.StatusDelivered {
		t.Fatalf("重启后变更组应恢复为 DELIVERED，得到 %s", restored.Status)
	}
	c2.pull("dev-2")
	c2.report("dev-1", twin.ReportRequest{Rev: restored.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "18"}})
	c2.report("dev-2", twin.ReportRequest{Rev: restored.DeviceResults["dev-2"].DesiredRev, Applied: twin.Config{"temp": "19"}})
	if got := c2.change(ch.ID).Status; got != twin.StatusEffective {
		t.Fatalf("重开后设备继续确认应完成变更，得到 %s", got)
	}
}

// 旧确认迟到时，只作为观察/历史，不改变当前目标或已被替代变更的成功记录。
func TestStaleReportPreservesSupersededHistory(t *testing.T) {
	c, _, _ := newHarness(t)
	old := c.publish(map[string]twin.Config{"dev-1": {"temp": "20"}})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: old.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	current := c.publish(map[string]twin.Config{"dev-1": {"temp": "26"}})
	c.report("dev-1", twin.ReportRequest{Rev: old.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	if got := c.change(old.ID).Status; got != twin.StatusSuperseded {
		t.Fatalf("旧变更应标记 SUPERSEDED，得到 %s", got)
	}
	if got := c.change(old.ID).DeviceResults["dev-1"].Status; got != twin.StatusEffective {
		t.Fatalf("旧设备成功确认应保留，得到 %s", got)
	}
	if v := c.twin("dev-1"); v.DesiredRev != current.DeviceResults["dev-1"].DesiredRev || v.ConfirmedConfig["temp"] != "20" {
		t.Fatalf("迟到旧确认不能改变当前期望或回退确认配置，得到 %+v", v)
	}
}

func TestConflictQueryForUnknownDeviceIsEmpty(t *testing.T) {
	c, _, _ := newHarness(t)
	if got := c.conflicts("ghost", false); len(got) != 0 {
		t.Fatalf("未知设备冲突列表应为空，得到 %d", len(got))
	}
}

func TestOverlappingGroupsQueueUntilPriorOperationFinishes(t *testing.T) {
	c, _, dbPath := newHarness(t)
	first := c.publish(map[string]twin.Config{
		"dev-a": {"temp": "20"},
		"dev-b": {"temp": "20"},
	})
	c.pull("dev-a")
	c.pull("dev-b")
	c.report("dev-a", twin.ReportRequest{Rev: first.DeviceResults["dev-a"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.report("dev-b", twin.ReportRequest{Rev: first.DeviceResults["dev-b"].DesiredRev, Applied: twin.Config{"temp": "20"}})

	second := c.publish(map[string]twin.Config{
		"dev-a": {"temp": "22"},
		"dev-b": {"temp": "22"},
	})
	c.pull("dev-a")
	c.pull("dev-b")
	c.do(http.MethodPost, "/devices/dev-b/offline", nil, nil)

	third := c.publish(map[string]twin.Config{
		"dev-a": {"temp": "23"},
		"dev-c": {"temp": "23"},
	})
	gotThird := c.change(third.ID)
	if gotThird.Status != twin.StatusQueued || gotThird.DeviceResults["dev-a"].Status != twin.StatusQueued || gotThird.DeviceResults["dev-a"].WaitForChange != second.ID {
		t.Fatalf("dev-a 应等待 second 完成，得到 %+v", gotThird)
	}
	if len(gotThird.ReadyDevices) != 1 || gotThird.ReadyDevices[0] != "dev-c" || len(gotThird.WaitingDevices) != 1 || gotThird.WaitingDevices[0] != "dev-a" {
		t.Fatalf("应展示可继续和必须等待的设备，得到 ready=%v waiting=%v", gotThird.ReadyDevices, gotThird.WaitingDevices)
	}

	c.pull("dev-c")
	c.report("dev-c", twin.ReportRequest{Rev: third.DeviceResults["dev-c"].DesiredRev, Applied: twin.Config{"temp": "23"}})
	if got := c.change(third.ID).Status; got != twin.StatusQueued {
		t.Fatalf("dev-c 已确认但 dev-a 未轮到时，third 应为 QUEUED，得到 %s", got)
	}

	c.report("dev-a", twin.ReportRequest{Rev: second.DeviceResults["dev-a"].DesiredRev, Applied: twin.Config{"temp": "22"}})
	dup := c.report("dev-a", twin.ReportRequest{Rev: second.DeviceResults["dev-a"].DesiredRev, Applied: twin.Config{"temp": "22"}})
	if stale, _ := dup["stale"].(bool); !stale {
		t.Fatalf("dev-a 已进入 third 后，second 的重复确认必须 stale，得到 %v", dup)
	}
	if got := c.change(second.ID).DeviceResults["dev-a"].Status; got != twin.StatusEffective {
		t.Fatalf("second 的 dev-a 成功记录不能被重复确认改写，得到 %s", got)
	}
	v := c.twin("dev-a")
	third = c.change(third.ID)
	if v.DesiredRev != third.DeviceResults["dev-a"].DesiredRev || v.DesiredConfig["temp"] != "23" || v.ConfirmedConfig["temp"] != "22" {
		t.Fatalf("重复旧确认不能改变当前目标或确认状态，得到 %+v", v)
	}

	c2 := newHarnessFromPath(t, dbPath)
	c2.pull("dev-b")
	c2.report("dev-b", twin.ReportRequest{Rev: second.DeviceResults["dev-b"].DesiredRev, Applied: twin.Config{"temp": "22"}})
	if got := c2.change(second.ID).Status; got != twin.StatusEffective {
		t.Fatalf("离线设备恢复并反馈后 second 才应 EFFECTIVE，得到 %s", got)
	}
	c2.pull("dev-a")
	c2.report("dev-a", twin.ReportRequest{Rev: third.DeviceResults["dev-a"].DesiredRev, Applied: twin.Config{"temp": "23"}})
	if got := c2.change(third.ID).Status; got != twin.StatusEffective {
		t.Fatalf("重启和排队接续后所有必要反馈齐全才应 EFFECTIVE，得到 %s", got)
	}
	for id, want := range map[string]string{"dev-a": "23", "dev-b": "22", "dev-c": "23"} {
		v := c2.twin(id)
		if v.Status != twin.StatusEffective || v.DesiredConfig["temp"] != want || v.ReportedConfig["temp"] != want || v.ConfirmedConfig["temp"] != want {
			t.Fatalf("%s 最终目标、观察和确认应均为 %s，得到 %+v", id, want, v)
		}
	}
}

func TestExplicitSupersedePreservesPartialDeviceHistory(t *testing.T) {
	c, _, _ := newHarness(t)
	old := c.publish(map[string]twin.Config{
		"dev-a": {"temp": "20"},
		"dev-b": {"temp": "20"},
	})
	c.pull("dev-a")
	c.pull("dev-b")
	c.report("dev-a", twin.ReportRequest{Rev: old.DeviceResults["dev-a"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.report("dev-b", twin.ReportRequest{
		Rev:      old.DeviceResults["dev-b"].DesiredRev,
		Applied:  twin.Config{},
		Rejected: twin.Config{"temp": "20"},
	})

	next := c.publishStrategy(map[string]twin.Config{
		"dev-a": {"temp": "24"},
		"dev-b": {"temp": "24"},
		"dev-c": {"temp": "24"},
	}, twin.ChangeStrategySupersede)
	historical := c.change(old.ID)
	if historical.Status != twin.StatusSuperseded {
		t.Fatalf("显式取代后旧组应为 SUPERSEDED，得到 %s", historical.Status)
	}
	if r := historical.DeviceResults["dev-a"]; r.Status != twin.StatusEffective || r.AppliedConfig["temp"] != "20" {
		t.Fatalf("已成功设备的实际确认应保留，得到 %+v", r)
	}
	if r := historical.DeviceResults["dev-b"]; r.Status != twin.StatusSuperseded || r.Rejected["temp"] != "20" {
		t.Fatalf("未完成设备的拒绝收尾结果应保留，得到 %+v", r)
	}
	if next.Status != twin.StatusPending || len(next.ReadyDevices) != 3 {
		t.Fatalf("显式取代后新组三台设备都应可继续，得到 %+v", next)
	}
}

func TestOverlappingReservationsExpireToCurrentlyValidTarget(t *testing.T) {
	c, _, dbPath := newHarness(t)
	t0 := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	c.clock(t0)

	r1 := c.schedule(map[string]twin.Config{"dev-1": {"temp": "18"}}, t0.Add(time.Hour), t0.Add(3*time.Hour))
	r2 := c.schedule(map[string]twin.Config{"dev-1": {"temp": "16"}}, t0.Add(2*time.Hour), t0.Add(4*time.Hour))
	if got := c.change(r1.ID).Status; got != twin.StatusScheduled {
		t.Fatalf("预约未到开始时间应为 SCHEDULED，得到 %s", got)
	}

	c.clock(t0.Add(time.Hour))
	r1 = c.change(r1.ID)
	if rev, cfg := c.pull("dev-1"); rev != r1.DeviceResults["dev-1"].DesiredRev || cfg["temp"] != "18" {
		t.Fatalf("r1 到点后应等待设备核实而非自动生效，rev=%d cfg=%v", rev, cfg)
	}
	c.report("dev-1", twin.ReportRequest{Rev: r1.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "18"}})
	if got := c.change(r1.ID).DeviceResults["dev-1"].Status; got != twin.StatusEffective {
		t.Fatalf("设备确认后 r1 才 EFFECTIVE，得到 %s", got)
	}

	c.clock(t0.Add(2 * time.Hour))
	c.do(http.MethodPost, "/devices/dev-1/offline", nil, nil)
	r1 = c.change(r1.ID)
	if got := r1.DeviceResults["dev-1"].DispatchState; got != "OVERLAPPED" {
		t.Fatalf("r1 被重叠预约覆盖但未结束，应为 OVERLAPPED，得到 %s", got)
	}
	r2 = c.change(r2.ID)
	if got := r2.Status; got != twin.StatusPending {
		t.Fatalf("r2 到点即使设备离线也应成为待核实目标，得到 %s", got)
	}

	c.clock(t0.Add(3 * time.Hour))
	r1 = c.change(r1.ID)
	if got := r1.Status; got != twin.StatusExpired {
		t.Fatalf("r1 到期应 EXPIRED，得到 %s", got)
	}
	if got := r1.DeviceResults["dev-1"].WaitReason; got != "WAITING_OVERLAPPING_RESERVATION_CONFIRMATION" {
		t.Fatalf("应说明仍等待重叠预约设备核实，得到 %q", got)
	}
	if r1.RestoresToChange != r2.ID {
		t.Fatalf("r1 到期应指向当时仍有效的 r2，得到 %q", r1.RestoresToChange)
	}

	c2 := newHarnessFromPath(t, dbPath)
	if got := c2.change(r1.ID).DeviceResults["dev-1"].WaitReason; got != "WAITING_OVERLAPPING_RESERVATION_CONFIRMATION" {
		t.Fatalf("保存重开后仍应说明设备为何等待，得到 %q", got)
	}
	c2.pull("dev-1")
	c2.report("dev-1", twin.ReportRequest{Rev: r2.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "16"}})
	r2 = c2.change(r2.ID)
	if got := r2.DeviceResults["dev-1"]; got.Status != twin.StatusEffective || got.AppliedConfig["temp"] != "16" {
		t.Fatalf("设备重连后的确认应明确归属 r2，得到 %+v", got)
	}
	if got := c2.change(r1.ID).DeviceResults["dev-1"].WaitReason; got != "" {
		t.Fatalf("r2 核实后 r1 不应再显示等待，得到 %q", got)
	}
	c2.clock(t0.Add(4 * time.Hour))
	r2 = c2.change(r2.ID)
	r2 = c2.change(r2.ID)
	restoreBase := c2.restoreFor(r2, "dev-1")
	if restoreBase.Targets["dev-1"]["temp"] != "" {
		t.Fatalf("r2 到期应回到预约创建时基线（空目标）而不是 r1 临时值，得到 %+v", restoreBase)
	}
	c2.pull("dev-1")
	dup := c2.report("dev-1", twin.ReportRequest{Rev: r2.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "16"}})
	if stale, _ := dup["stale"].(bool); !stale {
		t.Fatalf("恢复版本已成为当前目标后，重复 r2 回报必须 stale 且归属 r2 历史，得到 %v", dup)
	}
	c2.report("dev-1", twin.ReportRequest{Rev: restoreBase.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{}})
	if v := c2.twin("dev-1"); v.Status != twin.StatusEffective || len(v.DesiredConfig) != 0 || v.DesiredChangeID != restoreBase.ID {
		t.Fatalf("恢复必须经设备确认后才完成，得到 %+v", v)
	}
}

func TestShortenAndCancelReservationKeepObservedEffects(t *testing.T) {
	c, _, _ := newHarness(t)
	t0 := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	c.clock(t0)
	r := c.schedule(map[string]twin.Config{
		"dev-1": {"temp": "18"},
		"dev-2": {"temp": "18"},
	}, t0.Add(time.Hour), t0.Add(5*time.Hour))
	c.clock(t0.Add(time.Hour))
	r = c.change(r.ID)
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: r.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "18"}})

	c.shorten(r.ID, t0.Add(2*time.Hour), "dev-1")
	c.cancel(r.ID, "dev-2")
	r = c.change(r.ID)
	if got := r.DeviceResults["dev-2"].Status; got != twin.StatusCancelled || r.DeviceResults["dev-2"].EndReason != "CANCELLED" {
		t.Fatalf("取消剩余设备应为 CANCELLED，得到 %+v", r.DeviceResults["dev-2"])
	}
	if end := r.DeviceEndsAt["dev-1"]; !end.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("dev-1 应仅缩短有效期，得到 %s", end)
	}

	c.clock(t0.Add(2 * time.Hour))
	r = c.change(r.ID)
	if got := r.Status; got != twin.StatusExpired {
		t.Fatalf("已确认效果保留，但预约组状态应到期 EXPIRED，得到 %s", got)
	}
	restore := c.restoreFor(r, "dev-1")
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: restore.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{}})
	if got := c.twin("dev-1").Status; got != twin.StatusEffective {
		t.Fatalf("缩短后仍需安排并确认收尾，得到 %s", got)
	}
	if got := c.change(r.ID).DeviceResults["dev-1"].AppliedConfig["temp"]; got != "18" {
		t.Fatalf("已发生的临时效果历史必须保留，得到 %q", got)
	}
}
