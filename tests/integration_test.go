package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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

func (c testClient) publishWithMode(targets map[string]twin.Config, mode string) twin.Change {
	var ch twin.Change
	c.do(http.MethodPost, "/changes", map[string]any{"targets": targets, "mode": mode}, &ch)
	return ch
}

func (c testClient) verify(id string) twin.VerifyView {
	var v twin.VerifyView
	c.do(http.MethodGet, "/devices/"+id+"/verify", nil, &v)
	return v
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

// 变更排队：两次变更涉及同一台设备、另一台设备离线时，空闲设备立即继续，
// 忙碌设备排队等待先前操作结束；先前操作完成后排队变更自动激活，整组仍需全部确认。
func TestQueuedChangeWaitsForPriorOperation(t *testing.T) {
	c, _, _ := newHarness(t)
	first := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "20"},
		"dev-2": {"temp": "20"},
	})
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: first.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	c.do(http.MethodPost, "/devices/dev-2/offline", nil, nil) // dev-2 已下发未确认就离线

	second := c.publishWithMode(map[string]twin.Config{
		"dev-1": {"temp": "24"},
		"dev-2": {"temp": "24"},
	}, "queue")
	if second.DeviceResults["dev-2"].Status != twin.StatusQueued {
		t.Fatalf("dev-2 有未结束操作，应排队，得到 %+v", second.DeviceResults["dev-2"])
	}
	if second.DeviceResults["dev-1"].Status != twin.StatusPending || second.DeviceResults["dev-1"].DesiredRev == 0 {
		t.Fatalf("dev-1 空闲应立即继续，得到 %+v", second.DeviceResults["dev-1"])
	}
	v2 := c.twin("dev-2")
	if !v2.AwaitingFeedback || len(v2.QueuedChanges) != 1 || v2.QueuedChanges[0] != second.ID {
		t.Fatalf("dev-2 应显示等待先前反馈且有排队变更，得到 %+v", v2)
	}
	if v2.DesiredConfig["temp"] != "20" {
		t.Fatalf("排队不能覆盖 dev-2 未完成的目标，得到 %+v", v2)
	}
	if got := c.twin("dev-1").DesiredConfig["temp"]; got != "24" {
		t.Fatalf("dev-1 应已切换到新目标 temp=24，得到 %s", got)
	}
	if c.change(second.ID).Status == twin.StatusEffective {
		t.Fatal("dev-2 尚未参与，整组不能生效")
	}

	// dev-2 恢复：先完成先前操作，排队变更随后自动激活。
	rev2, cfg2 := c.pull("dev-2")
	if cfg2["temp"] != "20" {
		t.Fatalf("dev-2 恢复后仍应先拿到旧目标 temp=20，得到 rev=%d cfg=%v", rev2, cfg2)
	}
	c.report("dev-2", twin.ReportRequest{Rev: rev2, Applied: twin.Config{"temp": "20"}})
	done := c.change(first.ID)
	if done.EffectiveAt == nil || done.DeviceResults["dev-1"].Status != twin.StatusEffective || done.DeviceResults["dev-2"].Status != twin.StatusEffective {
		t.Fatalf("先前变更的实际确认结果必须保留，得到 %+v", done)
	}
	v2 = c.twin("dev-2")
	if len(v2.QueuedChanges) != 0 || v2.DesiredConfig["temp"] != "24" {
		t.Fatalf("先前操作结束后排队变更应激活，得到 %+v", v2)
	}
	rev2, cfg2 = c.pull("dev-2")
	if cfg2["temp"] != "24" {
		t.Fatalf("dev-2 应拉到排队激活的新目标 temp=24，得到 rev=%d cfg=%v", rev2, cfg2)
	}
	c.report("dev-1", twin.ReportRequest{Rev: second.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "24"}})
	if c.change(second.ID).Status == twin.StatusEffective {
		t.Fatal("dev-2 未确认前整组不能生效")
	}
	c.report("dev-2", twin.ReportRequest{Rev: rev2, Applied: twin.Config{"temp": "24"}})
	if got := c.change(second.ID).Status; got != twin.StatusEffective {
		t.Fatalf("所有设备确认后整组才生效，得到 %s", got)
	}
}

// 显式取代：被取代的变更保留实际已确认部分与收尾状态，不能改写成失败。
func TestExplicitSupersedePreservesClosure(t *testing.T) {
	c, _, _ := newHarness(t)
	first := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "20"},
		"dev-2": {"temp": "20"},
	})
	c.pull("dev-1")
	c.pull("dev-2")
	c.report("dev-1", twin.ReportRequest{Rev: first.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})

	second := c.publishWithMode(map[string]twin.Config{
		"dev-1": {"temp": "26"},
		"dev-2": {"temp": "26"},
	}, "supersede")
	old := c.change(first.ID)
	if old.Status != twin.StatusSuperseded || old.SupersededBy != second.ID || old.ClosureNote == "" {
		t.Fatalf("旧变更应标记被谁取代并留下收尾说明，得到 %+v", old)
	}
	if old.DeviceResults["dev-1"].Status != twin.StatusEffective || old.DeviceResults["dev-1"].AppliedConfig["temp"] != "20" {
		t.Fatalf("已确认部分必须原样保留，得到 %+v", old.DeviceResults["dev-1"])
	}
	r2 := old.DeviceResults["dev-2"]
	if r2.Status != twin.StatusSuperseded || r2.PriorStatus != twin.StatusDelivered {
		t.Fatalf("未确认设备的收尾状态应保留在 prior_status，得到 %+v", r2)
	}
	if got := c.twin("dev-2").DesiredConfig["temp"]; got != "26" {
		t.Fatalf("显式取代应立即切换目标，得到 %s", got)
	}
}

// 重复确认：同一版本的重复上报幂等，不重复产生冲突、不改变组状态。
func TestDuplicateConfirmationIsIdempotent(t *testing.T) {
	c, _, _ := newHarness(t)
	ch := c.publish(map[string]twin.Config{"dev-1": {"temp": "22"}})
	rev := ch.DeviceResults["dev-1"].DesiredRev
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: rev, Applied: twin.Config{"temp": "22"}})
	first := c.change(ch.ID)
	c.report("dev-1", twin.ReportRequest{Rev: rev, Applied: twin.Config{"temp": "22"}})
	second := c.change(ch.ID)
	if second.Status != twin.StatusEffective || !second.EffectiveAt.Equal(*first.EffectiveAt) {
		t.Fatalf("重复确认不能改变已生效结果，得到 %+v", second)
	}
	if got := c.conflicts("dev-1", true); len(got) != 0 {
		t.Fatalf("重复精确确认不应产生冲突，得到 %+v", got)
	}

	// 重复的部分拒绝也只保留一条未解决冲突。
	c.setDesired("dev-1", twin.Config{"temp": "22", "fan": "high"})
	c.pull("dev-1")
	v := c.twin("dev-1")
	rej := twin.ReportRequest{Rev: v.DesiredRev, Applied: twin.Config{"temp": "22"}, Rejected: twin.Config{"fan": "high"}}
	c.report("dev-1", rej)
	c.report("dev-1", rej)
	open := c.conflicts("dev-1", false)
	if len(open) != 1 || open[0].Reason != twin.ReasonRejected {
		t.Fatalf("重复拒绝应只记录一条冲突，得到 %+v", open)
	}
}

// 重启接续：排队中的变更随快照恢复，先前操作完成后仍能激活。
func TestQueuedChangeSurvivesRestart(t *testing.T) {
	c, _, dbPath := newHarness(t)
	first := c.publish(map[string]twin.Config{"dev-1": {"temp": "20"}})
	c.pull("dev-1")
	second := c.publishWithMode(map[string]twin.Config{"dev-1": {"temp": "25"}}, "queue")
	if second.DeviceResults["dev-1"].Status != twin.StatusQueued {
		t.Fatalf("已下发未确认时应排队，得到 %+v", second.DeviceResults["dev-1"])
	}

	c2 := newHarnessFromPath(t, dbPath)
	if got := c2.change(second.ID).Status; got != twin.StatusQueued {
		t.Fatalf("重启后排队变更应恢复为 QUEUED，得到 %s", got)
	}
	if v := c2.twin("dev-1"); len(v.QueuedChanges) != 1 {
		t.Fatalf("重启后设备队列应保留，得到 %+v", v)
	}
	c2.report("dev-1", twin.ReportRequest{Rev: first.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "20"}})
	v := c2.twin("dev-1")
	if v.DesiredConfig["temp"] != "25" || len(v.QueuedChanges) != 0 {
		t.Fatalf("重启后先前操作完成应激活排队变更，得到 %+v", v)
	}
	rev, _ := c2.pull("dev-1")
	c2.report("dev-1", twin.ReportRequest{Rev: rev, Applied: twin.Config{"temp": "25"}})
	if got := c2.change(second.ID).Status; got != twin.StatusEffective {
		t.Fatalf("排队变更确认后应生效，得到 %s", got)
	}
}

// 核对每台设备最终目标与真实观察状态。
func TestVerifyFinalTargetAndObservedState(t *testing.T) {
	c, _, _ := newHarness(t)
	ch := c.publish(map[string]twin.Config{"dev-1": {"temp": "21"}})
	c.pull("dev-1")
	v := c.verify("dev-1")
	if v.Final || v.ObservedMatchesDesired || v.ConfirmedMatchesDesired {
		t.Fatalf("未确认前核对不应通过，得到 %+v", v)
	}
	c.report("dev-1", twin.ReportRequest{Rev: ch.DeviceResults["dev-1"].DesiredRev, Applied: twin.Config{"temp": "21"}})
	v = c.verify("dev-1")
	if !v.Final || !v.ObservedMatchesDesired || !v.ConfirmedMatchesDesired || v.DesiredConfig["temp"] != "21" {
		t.Fatalf("确认后目标与观察应一致，得到 %+v", v)
	}
	// 新目标发布后，观察状态不再匹配最终目标。
	c.setDesired("dev-1", twin.Config{"temp": "23"})
	v = c.verify("dev-1")
	if v.Final || v.ObservedMatchesDesired {
		t.Fatalf("新目标未确认前核对不能通过，得到 %+v", v)
	}
}
