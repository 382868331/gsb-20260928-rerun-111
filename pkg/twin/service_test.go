package twin

import (
	"path/filepath"
	"testing"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "twin.json")
	svc, err := NewService(NewStore(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register("dev1"); err != nil {
		t.Fatal(err)
	}
	return svc, path
}

func TestOfflineConsecutiveChangesOnlyLatestTarget(t *testing.T) {
	svc, _ := newTestService(t)

	// 设备离线期间连续修改三次：不能排队成一串过时要求。
	if _, err := svc.SetDesired("dev1", map[string]string{"mode": "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDesired("dev1", map[string]string{"mode": "B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDesired("dev1", map[string]string{"mode": "C"}); err != nil {
		t.Fatal(err)
	}

	d, _ := svc.Get("dev1")
	got := d.Desired["mode"]
	if got.Value != "C" || got.Version != 3 || got.Status != StatusPending {
		t.Fatalf("offline edits collapsed wrong: %+v", got)
	}

	// 设备恢复后只拉到最新一版。
	resp, err := svc.Poll("dev1", PollRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Config) != 1 || resp.Config[0].Value != "C" || resp.Config[0].Version != 3 {
		t.Fatalf("device should only receive latest target: %+v", resp.Config)
	}

	d, _ = svc.Get("dev1")
	// 已下发不能显示成已生效。
	if d.Desired["mode"].Status != StatusDelivered {
		t.Fatalf("expected delivered, got %s", d.Desired["mode"].Status)
	}

	// 设备应用 v3 并带版本确认后才生效。
	if err := svc.Report("dev1", ReportRequest{Items: []ReportItem{{
		Key: "mode", Value: "C", Version: 3, Applied: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	d, _ = svc.Get("dev1")
	if d.Desired["mode"].Status != StatusEffective {
		t.Fatalf("expected effective, got %s", d.Desired["mode"].Status)
	}
	c := d.Confirmed["mode"]
	if c.Value != "C" || c.Version != 3 {
		t.Fatalf("confirmed state wrong: %+v", c)
	}
}

func TestStaleAckCannotOverrideLatestTarget(t *testing.T) {
	svc, _ := newTestService(t)

	svc.SetDesired("dev1", map[string]string{"mode": "A"}) // v1
	svc.Poll("dev1", PollRequest{})
	svc.SetDesired("dev1", map[string]string{"mode": "B"}) // v2
	svc.SetDesired("dev1", map[string]string{"mode": "C"}) // v3

	// 迟到的 v1 确认：旧确认不能盖掉最新目标。
	err := svc.Report("dev1", ReportRequest{Items: []ReportItem{{
		Key: "mode", Value: "A", Version: 1, Applied: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := svc.Get("dev1")
	if d.Desired["mode"].Status == StatusEffective || d.Desired["mode"].Value != "C" {
		t.Fatalf("stale ack must not confirm target: %+v", d.Desired["mode"])
	}
	if _, ok := d.Confirmed["mode"]; ok {
		t.Fatal("confirmed state must not be written by stale ack")
	}
	conflicts, _ := svc.Conflicts("dev1")
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictStaleAck {
		t.Fatalf("expected one stale_ack conflict, got %+v", conflicts)
	}

	// 设备恢复后仍能拉到最新 v3。
	resp, _ := svc.Poll("dev1", PollRequest{})
	if len(resp.Config) != 1 || resp.Config[0].Version != 3 {
		t.Fatalf("latest target lost after stale ack: %+v", resp.Config)
	}
}

func TestPartialRejection(t *testing.T) {
	svc, _ := newTestService(t)

	svc.SetDesired("dev1", map[string]string{
		"brightness": "80",
		"interval":   "5",
	})
	svc.Poll("dev1", PollRequest{})

	// 设备接受 brightness，拒绝 interval（值保持旧的 "10"）。
	err := svc.Report("dev1", ReportRequest{Items: []ReportItem{
		{Key: "brightness", Value: "80", Version: 1, Applied: true},
		{Key: "interval", Value: "10", Version: 1, Applied: false, Reason: "out of range"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := svc.Get("dev1")
	if d.Desired["brightness"].Status != StatusEffective {
		t.Fatalf("brightness should be effective, got %s", d.Desired["brightness"].Status)
	}
	if d.Desired["interval"].Status != StatusRejected {
		t.Fatalf("interval should be rejected, got %s", d.Desired["interval"].Status)
	}
	if c, ok := d.Confirmed["interval"]; ok {
		t.Fatalf("rejected key must not be confirmed: %+v", c)
	}
	conflicts, _ := svc.Conflicts("dev1")
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictRejected || conflicts[0].Key != "interval" {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
}

func TestVersionlessReportIsNotConfirmation(t *testing.T) {
	svc, _ := newTestService(t)

	svc.SetDesired("dev1", map[string]string{"mode": "A"}) // v1
	svc.Poll("dev1", PollRequest{})

	// 设备只上报实际值（不带版本），即使值相同也不能算已生效。
	if err := svc.Report("dev1", ReportRequest{
		Snapshot: map[string]string{"mode": "A"},
	}); err != nil {
		t.Fatal(err)
	}
	d, _ := svc.Get("dev1")
	if d.Desired["mode"].Status != StatusDelivered {
		t.Fatalf("versionless report must stay delivered, got %s", d.Desired["mode"].Status)
	}
	if d.Reported["mode"].Value != "A" {
		t.Fatalf("reported state not stored")
	}
	if _, ok := d.Confirmed["mode"]; ok {
		t.Fatal("reported must not become confirmed")
	}
}

func TestRestartPreservesInFlightChange(t *testing.T) {
	svc, path := newTestService(t)

	svc.SetDesired("dev1", map[string]string{"mode": "A"})
	// mode 已下发未确认；模拟服务重启。
	svc.Poll("dev1", PollRequest{})
	// 重启前又追加了一项还没来得及下发的新配置。
	svc.SetDesired("dev1", map[string]string{"brightness": "50"})

	svc2, err := NewService(NewStore(path))
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc2.Get("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Desired["mode"].Status != StatusDelivered || d.Desired["mode"].Version != 1 {
		t.Fatalf("delivered change lost across restart: %+v", d.Desired["mode"])
	}
	if d.Desired["brightness"].Status != StatusPending {
		t.Fatalf("brightness state lost across restart: %+v", d.Desired["brightness"])
	}

	// 重启后设备恢复，旧的未完成变更继续走完确认流程。
	if err := svc2.Report("dev1", ReportRequest{Items: []ReportItem{{
		Key: "mode", Value: "A", Version: 1, Applied: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	d, _ = svc2.Get("dev1")
	if d.Desired["mode"].Status != StatusEffective || d.Confirmed["mode"].Version != 1 {
		t.Fatalf("change not completed after restart: %+v", d)
	}
}

func TestIdempotentEffectiveValue(t *testing.T) {
	svc, _ := newTestService(t)

	svc.SetDesired("dev1", map[string]string{"mode": "A"})
	svc.Poll("dev1", PollRequest{})
	svc.Report("dev1", ReportRequest{Items: []ReportItem{{
		Key: "mode", Value: "A", Version: 1, Applied: true,
	}}})
	// 再次设置相同值不应产生新版本/重复下发。
	svc.SetDesired("dev1", map[string]string{"mode": "A"})
	resp, _ := svc.Poll("dev1", PollRequest{})
	if len(resp.Config) != 0 {
		t.Fatalf("unchanged effective value should not be redelivered: %+v", resp.Config)
	}
}
