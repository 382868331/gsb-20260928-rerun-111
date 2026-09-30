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

func TestConflictQueryForUnknownDeviceIsEmpty(t *testing.T) {
	c, _, _ := newHarness(t)
	if got := c.conflicts("ghost", false); len(got) != 0 {
		t.Fatalf("未知设备冲突列表应为空，得到 %d", len(got))
	}
}

func (c testClient) preview(id string, cfg twin.Config) twin.ConfigDiff {
	var resp struct {
		Diff twin.ConfigDiff `json:"diff"`
	}
	c.do(http.MethodPost, "/devices/"+id+"/preview", cfg, &resp)
	return resp.Diff
}

func (c testClient) changes(id string) []twin.Change {
	var resp struct {
		Changes []twin.Change `json:"changes"`
	}
	c.do(http.MethodGet, "/devices/"+id+"/changes", nil, &resp)
	return resp.Changes
}

func (c testClient) publish(spec map[string]twin.Config) twin.TargetView {
	var v twin.TargetView
	c.do(http.MethodPost, "/targets", map[string]any{"devices": spec}, &v)
	return v
}

func (c testClient) target(id string) twin.TargetView {
	var v twin.TargetView
	c.do(http.MethodGet, "/targets/"+id, nil, &v)
	return v
}

func (c testClient) compensate(id string, spec map[string]twin.Config) twin.TargetView {
	var v twin.TargetView
	body := map[string]any{}
	if spec != nil {
		body["devices"] = spec
	}
	c.do(http.MethodPost, "/targets/"+id+"/compensate", body, &v)
	return v
}

// 预演只报告差异，不发布、不推进版本。
func TestPreviewDoesNotPublish(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setDesired("dev-1", twin.Config{"temp": "22", "mode": "eco"})
	diff := c.preview("dev-1", twin.Config{"temp": "24", "fan": "high"})
	if diff.Change["temp"].To != "24" || diff.Add["fan"] != "high" || diff.Remove["mode"] != "eco" {
		t.Fatalf("预演差异不正确：%+v", diff)
	}
	v := c.twin("dev-1")
	if v.DesiredRev != 1 || v.DesiredConfig["temp"] != "22" {
		t.Fatalf("预演不能改变期望，得到 rev=%d cfg=%v", v.DesiredRev, v.DesiredConfig)
	}
}

// 多设备相关配置整体生效：一台成功不能代表另一台；全部确认后整体才生效。
func TestTargetAllOrNothingAcrossDevices(t *testing.T) {
	c, _, _ := newHarness(t)
	tv := c.publish(map[string]twin.Config{
		"dev-1": {"temp": "22"},
		"dev-2": {"temp": "25"},
	})
	if tv.Overall != twin.StatusPending {
		t.Fatalf("发布后整体应为 PENDING，得到 %s", tv.Overall)
	}
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "22"}})
	tv = c.target(tv.ID)
	if tv.Overall != twin.StatusPending {
		t.Fatalf("仅 dev-1 生效、dev-2 未下发时整体应受最未完成设备约束为 PENDING，得到 %s", tv.Overall)
	}
	// dev-1 的成功不能算到 dev-2 头上。
	v2 := c.twin("dev-2")
	if v2.ConfirmedRev != 0 || v2.Status != twin.StatusPending {
		t.Fatalf("dev-2 不应受 dev-1 成功影响，得到 %+v", v2)
	}
	c.pull("dev-2")
	tv = c.target(tv.ID)
	if tv.Overall != twin.StatusDelivered {
		t.Fatalf("dev-2 仅已下发时整体仍应为 DELIVERED，得到 %s", tv.Overall)
	}
	c.report("dev-2", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "25"}})
	tv = c.target(tv.ID)
	if tv.Overall != twin.StatusEffective {
		t.Fatalf("两台都确认后整体才应为 EFFECTIVE，得到 %s", tv.Overall)
	}
}

// 部分接受只保留实际状态；失败后发起补偿目标，补偿也需设备确认。
func TestCompensationNeedsConfirmation(t *testing.T) {
	c, _, _ := newHarness(t)
	tv := c.publish(map[string]twin.Config{"dev-1": {"temp": "22", "fan": "high"}})
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{
		Rev:      1,
		Applied:  twin.Config{"temp": "22"},
		Rejected: twin.Config{"fan": "high"},
	})
	tv = c.target(tv.ID)
	if tv.Overall != twin.StatusPartial {
		t.Fatalf("部分拒绝后整体应为 PARTIAL，得到 %s", tv.Overall)
	}
	if v := c.twin("dev-1"); v.ConfirmedConfig["fan"] != "" || v.ConfirmedConfig["temp"] != "22" {
		t.Fatalf("确认状态只能包含实际接受项，得到 %v", v.ConfirmedConfig)
	}

	// 补偿目标默认回退到最近一次已确认配置；它是新版本，必须重新下发与确认。
	comp := c.compensate(tv.ID, nil)
	if comp.Kind != twin.ChangeCompensation || comp.Compensates != tv.ID || comp.Overall != twin.StatusPending {
		t.Fatalf("补偿目标应为新的待确认目标，得到 %+v", comp)
	}
	if v := c.twin("dev-1"); v.Status != twin.StatusPending || v.DesiredRev != 2 {
		t.Fatalf("补偿发布后不能直接生效，得到 %+v", v)
	}
	c.pull("dev-1")
	if v := c.twin("dev-1"); v.Status != twin.StatusDelivered {
		t.Fatalf("补偿仅下发不应视为生效，得到 %s", v.Status)
	}
	c.report("dev-1", twin.ReportRequest{Rev: 2, Applied: twin.Config{"temp": "22"}})
	comp = c.target(comp.ID)
	if comp.Overall != twin.StatusEffective {
		t.Fatalf("补偿经设备确认后才应 EFFECTIVE，得到 %s", comp.Overall)
	}
}

// 离线连改、旧确认迟到、设备与服务重启：被替代版本记录全部保留，重开后继续未完成操作。
func TestOfflineLateAckRestartKeepsSupersededChanges(t *testing.T) {
	c, _, dbPath := newHarness(t)
	c.setDesired("dev-1", twin.Config{"temp": "20"}) // rev1 已生效
	c.pull("dev-1")
	c.report("dev-1", twin.ReportRequest{Rev: 1, Applied: twin.Config{"temp": "20"}})
	c.setDesired("dev-1", twin.Config{"temp": "24"}) // rev2 已下发未确认
	c.pull("dev-1")
	c.do(http.MethodPost, "/devices/dev-1/offline", nil, nil)
	c.setDesired("dev-1", twin.Config{"temp": "26"}) // rev3 离线修改
	c.setDesired("dev-1", twin.Config{"temp": "28"}) // rev4 离线修改

	// 设备重启（相当于重新打开）：服务侧仍保留 rev2 已下发的未完成状态。
	if v := c.twin("dev-1"); v.Status != twin.StatusPending {
		t.Fatalf("最新 rev4 尚未下发应为 PENDING，得到 %s", v.Status)
	}

	// 旧确认迟到（针对已被替代的 rev2）：不能让 rev4 生效。
	c.report("dev-1", twin.ReportRequest{Rev: 2, Applied: twin.Config{"temp": "24"}})
	if v := c.twin("dev-1"); v.Status != twin.StatusPending || v.ConfirmedRev != 1 {
		t.Fatalf("旧版本迟到确认不能推进当前生效，得到 %+v", v)
	}

	c.pull("dev-1")
	if v := c.twin("dev-1"); v.Status != twin.StatusDelivered {
		t.Fatalf("恢复后拉取 rev4 应为 DELIVERED，得到 %s", v.Status)
	}

	// 服务重启后继续未完成操作。
	store2, err := twin.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c2 := testClient{t: t, h: server.New(store2).Routes()}
	if v := c2.twin("dev-1"); v.Status != twin.StatusDelivered || v.ConfirmedRev != 1 {
		t.Fatalf("重启后应仍是 rev4 已下发待确认，得到 %+v", v)
	}
	c2.report("dev-1", twin.ReportRequest{Rev: 4, Applied: twin.Config{"temp": "28"}})
	if v := c2.twin("dev-1"); v.Status != twin.StatusEffective {
		t.Fatalf("重启后继续完成 rev4 应 EFFECTIVE，得到 %s", v.Status)
	}

	// 被替代的 rev2、从未下发的 rev3 仍保留在变更记录中。
	ch := c2.changes("dev-1")
	if len(ch) != 4 {
		t.Fatalf("应保留 4 次发布记录，得到 %d: %+v", len(ch), ch)
	}
	status := map[int64]twin.Status{}
	for _, x := range ch {
		status[x.Rev] = x.Status
	}
	if status[1] != twin.StatusEffective || status[2] != twin.StatusSuperseded || status[3] != twin.StatusSuperseded || status[4] != twin.StatusEffective {
		t.Fatalf("变更记录状态不正确：%v", status)
	}
	if len(c2.conflicts("dev-1", true)) == 0 {
		t.Fatal("被替代版本仍应保留冲突历史")
	}
}
