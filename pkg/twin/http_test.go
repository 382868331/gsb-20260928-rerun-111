package twin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 用 httptest.ResponseRecorder 在进程内驱动 HTTP handler，
// 行为与真实 HTTP 服务一致，且不依赖网络 socket。
func setupTestHandler(t *testing.T) (*Service, string, http.Handler) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "twin.json")
	svc, err := NewService(NewStore(path))
	if err != nil {
		t.Fatal(err)
	}
	return svc, path, NewServer(svc).Handler()
}

func doJSON(t *testing.T, h http.Handler, method, url string, body any, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, url, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s -> %d: %s", method, url, resp.StatusCode, rec.Body.String())
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s: %v", method, url, err)
		}
	}
}

func postJSON(t *testing.T, h http.Handler, url string, body any, out any) {
	doJSON(t, h, http.MethodPost, url, body, out)
}

func putJSON(t *testing.T, h http.Handler, url string, body any) {
	doJSON(t, h, http.MethodPut, url, body, nil)
}

func getJSON(t *testing.T, h http.Handler, url string, out any) {
	doJSON(t, h, http.MethodGet, url, nil, out)
}

// TestEndToEnd 模拟完整流程：
//  1. 离线期间连续修改 mode A->B->C，设备恢复后只执行 C；
//  2. 设备部分拒绝 interval；
//  3. 设备先发来 v1 的迟到确认，再发来正确的 v3 确认；
//  4. 服务重启后未完成/已确认状态均保留；
//  5. 无版本快照上报不能把 delivered 变成 effective。
func TestEndToEnd(t *testing.T) {
	_, dbPath, h := setupTestHandler(t)
	dev := "/api/devices/vdev-1"

	postJSON(t, h, "/api/devices", map[string]string{"id": "vdev-1"}, nil)

	// 设备启动，先上报本地真实快照（无版本）。
	postJSON(t, h, dev+"/report", ReportRequest{Snapshot: map[string]string{"interval": "10"}}, nil)

	// 离线期间的连续配置变更。
	putJSON(t, h, dev+"/desired", SetDesiredRequest{Config: map[string]string{"mode": "A", "interval": "5"}})
	putJSON(t, h, dev+"/desired", SetDesiredRequest{Config: map[string]string{"mode": "B"}})
	putJSON(t, h, dev+"/desired", SetDesiredRequest{Config: map[string]string{"mode": "C"}})

	// 设备恢复并拉取：只能拿到 mode v3 和 interval v1。
	var polled PollResponse
	postJSON(t, h, dev+"/poll", PollRequest{}, &polled)
	if len(polled.Config) != 2 {
		t.Fatalf("expected 2 pending items, got %+v", polled.Config)
	}
	for _, item := range polled.Config {
		switch item.Key {
		case "mode":
			if item.Version != 3 || item.Value != "C" {
				t.Fatalf("device must only see latest mode, got %+v", item)
			}
		case "interval":
			if item.Version != 1 || item.Value != "5" {
				t.Fatalf("interval wrong: %+v", item)
			}
		default:
			t.Fatalf("unexpected key %s", item.Key)
		}
	}

	// 已下发不等于已生效。
	var twin DeviceTwin
	getJSON(t, h, dev, &twin)
	if twin.Desired["mode"].Status != StatusDelivered || twin.Desired["interval"].Status != StatusDelivered {
		t.Fatalf("after poll items must be delivered: %+v", twin.Desired)
	}

	// 设备实际应用 mode=C，但网络层先到了 v1 的迟到旧确认。
	postJSON(t, h, dev+"/report", ReportRequest{Items: []ReportItem{
		{Key: "mode", Value: "A", Version: 1, Applied: true},
	}}, nil)
	getJSON(t, h, dev, &twin)
	if twin.Desired["mode"].Status == StatusEffective {
		t.Fatal("stale ack must not mark effective")
	}
	if _, ok := twin.Confirmed["mode"]; ok {
		t.Fatal("stale ack must not write confirmed")
	}

	// interval 被设备拒绝；mode 随后发来正确的 v3 确认。
	postJSON(t, h, dev+"/report", ReportRequest{Items: []ReportItem{
		{Key: "interval", Value: "10", Version: 1, Applied: false, Reason: "out of range"},
		{Key: "mode", Value: "C", Version: 3, Applied: true},
	}}, nil)

	getJSON(t, h, dev, &twin)
	if twin.Desired["mode"].Status != StatusEffective {
		t.Fatalf("mode should be effective, got %s", twin.Desired["mode"].Status)
	}
	if twin.Confirmed["mode"].Version != 3 || twin.Confirmed["mode"].Value != "C" {
		t.Fatalf("confirmed wrong: %+v", twin.Confirmed["mode"])
	}
	if twin.Desired["interval"].Status != StatusRejected {
		t.Fatalf("interval should be rejected, got %s", twin.Desired["interval"].Status)
	}
	if twin.Reported["interval"].Value != "10" {
		t.Fatalf("reported actual value should stay 10")
	}

	// 冲突可查询：1 条 stale_ack + 1 条 rejected。
	var conflicts []Conflict
	getJSON(t, h, dev+"/conflicts", &conflicts)
	kinds := map[string]int{}
	for _, cf := range conflicts {
		kinds[fmt.Sprintf("%s:%s", cf.Key, cf.Kind)]++
	}
	if kinds["mode:stale_ack"] != 1 || kinds["interval:rejected"] != 1 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}

	// 再下发一项设备尚未拉取的变更，然后用同一状态文件重新加载模拟重启。
	putJSON(t, h, dev+"/desired", SetDesiredRequest{Config: map[string]string{"brightness": "90"}})
	getJSON(t, h, dev, &twin)
	if twin.Desired["brightness"].Status != StatusPending {
		t.Fatalf("new change should be pending, got %s", twin.Desired["brightness"].Status)
	}

	restarted, err := NewService(NewStore(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	d, err := restarted.Get("vdev-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Desired["brightness"].Status != StatusPending || d.Desired["brightness"].Version != 1 {
		t.Fatalf("pending change must survive restart: %+v", d.Desired["brightness"])
	}
	if d.Desired["mode"].Status != StatusEffective || d.Confirmed["mode"].Version != 3 {
		t.Fatalf("confirmed state must survive restart: %+v", d)
	}
	if d.Desired["interval"].Status != StatusRejected {
		t.Fatalf("rejected state must survive restart: %+v", d.Desired["interval"])
	}
	if len(d.Conflicts) != 2 {
		t.Fatalf("conflict log must survive restart: %+v", d.Conflicts)
	}
}
