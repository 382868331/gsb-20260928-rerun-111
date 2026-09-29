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
