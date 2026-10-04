package tests

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"twindemo/internal/server"
	"twindemo/internal/twin"
)

// virtualClock 让测试用虚拟时间推进预约的生效与到期。
type virtualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *virtualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *virtualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var reservationBase = time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)

func newReservationHarness(t *testing.T) (testClient, *virtualClock, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.json")
	clock := &virtualClock{t: reservationBase}
	return reservationClient(t, dbPath, clock), clock, dbPath
}

func reservationClient(t *testing.T, dbPath string, clock *virtualClock) testClient {
	t.Helper()
	store, err := twin.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(store)
	srv.SetClock(clock.now)
	return testClient{t: t, h: srv.Routes()}
}

func (c testClient) createReservation(targets map[string]twin.Config, start, end time.Time) twin.ReservationView {
	var v twin.ReservationView
	c.do(http.MethodPost, "/reservations", map[string]any{
		"targets":  targets,
		"start_at": start.Format(time.RFC3339),
		"end_at":   end.Format(time.RFC3339),
	}, &v)
	return v
}

func (c testClient) reservation(id string) twin.ReservationView {
	var v twin.ReservationView
	c.do(http.MethodGet, "/reservations/"+id, nil, &v)
	return v
}

func (c testClient) shortenReservation(id string, end time.Time) twin.ReservationView {
	var v twin.ReservationView
	c.do(http.MethodPatch, "/reservations/"+id, map[string]any{
		"end_at": end.Format(time.RFC3339),
	}, &v)
	return v
}

func (c testClient) cancelReservation(id string, devices ...string) twin.ReservationView {
	var v twin.ReservationView
	c.do(http.MethodPost, "/reservations/"+id+"/cancel", map[string]any{"devices": devices}, &v)
	return v
}

func devicePhase(v twin.ReservationView, deviceID string) twin.ReservationDeviceView {
	for _, d := range v.Devices {
		if d.DeviceID == deviceID {
			return d
		}
	}
	return twin.ReservationDeviceView{}
}

// confirmEffective 让设备拉取并精确上报当前期望。
func (c testClient) confirmEffective(id string) twin.Config {
	rev, cfg := c.pull(id)
	c.report(id, twin.ReportRequest{Rev: rev, Applied: cfg})
	return cfg
}

func TestOverlappingReservationsRestoreToStillValidTarget(t *testing.T) {
	c, clock, _ := newReservationHarness(t)

	c.setDesired("dev-1", twin.Config{"temp": "22", "mode": "eco"})
	c.confirmEffective("dev-1")

	start := reservationBase.Add(time.Hour) // 22:00
	r1 := c.createReservation(map[string]twin.Config{"dev-1": {"temp": "18"}}, start, start.Add(8*time.Hour))
	r2 := c.createReservation(map[string]twin.Config{"dev-1": {"temp": "16"}}, start.Add(2*time.Hour), start.Add(6*time.Hour))

	if got := devicePhase(r1, "dev-1").Phase; got != twin.PhaseWaitingStart {
		t.Fatalf("开始前应为 WAITING_START，实际 %s", got)
	}

	// R1 到点：应当生效，但时间到不代表设备已完成。
	clock.advance(time.Hour)
	v := c.reservation(r1.ID)
	if v.Status != twin.ReservationActive {
		t.Fatalf("R1 应为 ACTIVE，实际 %s", v.Status)
	}
	if got := devicePhase(v, "dev-1").Phase; got != twin.PhaseDue {
		t.Fatalf("到点未确认应为 DUE，实际 %s", got)
	}
	if cfg := c.confirmEffective("dev-1"); cfg["temp"] != "18" {
		t.Fatalf("R1 生效配置应为 18，实际 %v", cfg)
	}
	if got := devicePhase(c.reservation(r1.ID), "dev-1").Phase; got != twin.PhaseConfirmed {
		t.Fatalf("确认后应为 CONFIRMED，实际 %s", got)
	}

	// R2 到点，与 R1 重叠，覆盖为 16。
	clock.advance(2 * time.Hour)
	if cfg := c.confirmEffective("dev-1"); cfg["temp"] != "16" {
		t.Fatalf("R2 生效配置应为 16，实际 %v", cfg)
	}

	// R2 到期：恢复到当时仍有效的 R1（18），而不是创建时的旧值 22。
	clock.advance(4 * time.Hour)
	v = c.reservation(r2.ID)
	if v.Status != twin.ReservationRestoring {
		t.Fatalf("R2 到期应为 RESTORING，实际 %s", v.Status)
	}
	if got := devicePhase(v, "dev-1").Phase; got != twin.PhaseRestoreDue {
		t.Fatalf("R2 到期未确认应为 RESTORE_DUE，实际 %s", got)
	}
	if cfg := c.confirmEffective("dev-1"); cfg["temp"] != "18" {
		t.Fatalf("R2 到期应恢复到 R1 的 18，实际 %v", cfg)
	}
	if v = c.reservation(r2.ID); v.Status != twin.ReservationCompleted {
		t.Fatalf("R2 恢复确认后应为 COMPLETED，实际 %s", v.Status)
	}

	// R1 到期：恢复到底层基线 22。
	clock.advance(2 * time.Hour)
	if cfg := c.confirmEffective("dev-1"); cfg["temp"] != "22" || cfg["mode"] != "eco" {
		t.Fatalf("R1 到期应恢复基线 22/eco，实际 %v", cfg)
	}
	if v = c.reservation(r1.ID); v.Status != twin.ReservationCompleted {
		t.Fatalf("R1 恢复确认后应为 COMPLETED，实际 %s", v.Status)
	}
	if tv := c.twin("dev-1"); tv.Status != twin.StatusEffective {
		t.Fatalf("最终设备应为 EFFECTIVE，实际 %s", tv.Status)
	}
}

func TestReservationOfflineLateAndDuplicateReports(t *testing.T) {
	c, clock, _ := newReservationHarness(t)

	c.setDesired("dev-2", twin.Config{"temp": "22"})
	c.confirmEffective("dev-2")
	c.do(http.MethodPost, "/devices/dev-2/offline", nil, nil)

	start := reservationBase.Add(time.Hour)
	r := c.createReservation(map[string]twin.Config{"dev-2": {"temp": "18"}}, start, start.Add(2*time.Hour))

	// 生效时设备离线：应当生效但等待重连，不能当作已完成。
	clock.advance(time.Hour)
	v := c.reservation(r.ID)
	dv := devicePhase(v, "dev-2")
	if dv.Phase != twin.PhaseDue || dv.WaitReason == "" {
		t.Fatalf("离线到点应为 DUE 且带等待原因，实际 %+v", dv)
	}

	// 设备一直离线到期：自动安排恢复，仍等待设备核实。
	clock.advance(2 * time.Hour)
	v = c.reservation(r.ID)
	dv = devicePhase(v, "dev-2")
	if v.Status != twin.ReservationRestoring || dv.Phase != twin.PhaseRestoreDue {
		t.Fatalf("到期离线应为 RESTORING/RESTORE_DUE，实际 %s/%s", v.Status, dv.Phase)
	}

	// 设备重连，先迟到上报预约生效版本：归属生效变更，不影响恢复收尾。
	resp := c.report("dev-2", twin.ReportRequest{Rev: 2, Applied: twin.Config{"temp": "18"}})
	if stale, _ := resp["stale"].(bool); !stale {
		t.Fatalf("迟到上报应标记 stale，实际 %v", resp)
	}
	act := c.change(dv.ActivateChangeID)
	if got := act.DeviceResults["dev-2"].ReportedRev; got != 2 {
		t.Fatalf("迟到确认应归属生效变更 %s，实际 reported_rev=%d", dv.ActivateChangeID, got)
	}
	if got := devicePhase(c.reservation(r.ID), "dev-2").Phase; got != twin.PhaseRestoreDue {
		t.Fatalf("迟到确认不能完成恢复，实际 %s", got)
	}

	// 拉取恢复目标并确认；重复上报同一版本不产生回退。
	rev, cfg := c.pull("dev-2")
	if cfg["temp"] != "22" {
		t.Fatalf("恢复目标应为 22，实际 %v", cfg)
	}
	c.report("dev-2", twin.ReportRequest{Rev: rev, Applied: cfg})
	c.report("dev-2", twin.ReportRequest{Rev: rev, Applied: cfg})
	v = c.reservation(r.ID)
	if v.Status != twin.ReservationCompleted {
		t.Fatalf("恢复确认后应为 COMPLETED，实际 %s", v.Status)
	}
	if tv := c.twin("dev-2"); tv.ConfirmedRev != rev || tv.Status != twin.StatusEffective {
		t.Fatalf("重复上报后确认版本应为 %d 且 EFFECTIVE，实际 %+v", rev, tv)
	}
}

func TestShortenReservationWindow(t *testing.T) {
	c, clock, _ := newReservationHarness(t)

	c.setDesired("dev-3", twin.Config{"temp": "22"})
	c.confirmEffective("dev-3")

	start := reservationBase.Add(time.Hour)
	r := c.createReservation(map[string]twin.Config{"dev-3": {"temp": "18"}}, start, start.Add(8*time.Hour))
	clock.advance(time.Hour)
	c.confirmEffective("dev-3")

	// 缩短到未来：到达新终点才到期。
	r = c.shortenReservation(r.ID, start.Add(2*time.Hour))
	if r.Status != twin.ReservationActive {
		t.Fatalf("缩短后未到点应仍 ACTIVE，实际 %s", r.Status)
	}
	clock.advance(2 * time.Hour)
	if v := c.reservation(r.ID); v.Status != twin.ReservationRestoring {
		t.Fatalf("到达缩短后的终点应到期，实际 %s", v.Status)
	}
	if cfg := c.confirmEffective("dev-3"); cfg["temp"] != "22" {
		t.Fatalf("缩短到期应恢复 22，实际 %v", cfg)
	}

	// 缩短到过去：立即到期并安排收尾。
	r2 := c.createReservation(map[string]twin.Config{"dev-3": {"temp": "16"}}, reservationBase.Add(4*time.Hour), reservationBase.Add(9*time.Hour))
	clock.advance(2 * time.Hour) // 进入 R2 窗口
	c.confirmEffective("dev-3")
	r2 = c.shortenReservation(r2.ID, reservationBase.Add(3*time.Hour))
	if r2.Status != twin.ReservationRestoring {
		t.Fatalf("缩短到过去应立即到期，实际 %s", r2.Status)
	}
	if got := devicePhase(r2, "dev-3").Phase; got != twin.PhaseRestoreDue {
		t.Fatalf("立即到期后应为 RESTORE_DUE，实际 %s", got)
	}
}

func TestCancelRemainingDevicesKeepsEffectsAndWrapsUp(t *testing.T) {
	c, clock, _ := newReservationHarness(t)

	for _, id := range []string{"dev-a", "dev-b"} {
		c.setDesired(id, twin.Config{"temp": "22"})
		c.confirmEffective(id)
	}
	start := reservationBase.Add(time.Hour)
	r := c.createReservation(map[string]twin.Config{
		"dev-a": {"temp": "18"},
		"dev-b": {"temp": "18"},
	}, start, start.Add(8*time.Hour))
	clock.advance(time.Hour)
	c.confirmEffective("dev-a")
	c.confirmEffective("dev-b")

	// 执行途中取消 dev-b：已生效效果保留，安排恢复收尾；dev-a 不受影响。
	r = c.cancelReservation(r.ID, "dev-b")
	if got := devicePhase(r, "dev-b").Phase; got != twin.PhaseRestoreDue {
		t.Fatalf("取消已生效设备应安排收尾 RESTORE_DUE，实际 %s", got)
	}
	if got := devicePhase(r, "dev-a").Phase; got != twin.PhaseConfirmed {
		t.Fatalf("未取消设备应保持 CONFIRMED，实际 %s", got)
	}
	if tv := c.twin("dev-b"); tv.DesiredConfig["temp"] != "22" {
		t.Fatalf("dev-b 恢复目标应为 22，实际 %v", tv.DesiredConfig)
	}
	if tv := c.twin("dev-a"); tv.DesiredConfig["temp"] != "18" {
		t.Fatalf("dev-a 应继续保持 18，实际 %v", tv.DesiredConfig)
	}
	c.confirmEffective("dev-b")
	if got := devicePhase(c.reservation(r.ID), "dev-b").Phase; got != twin.PhaseRestored {
		t.Fatalf("dev-b 恢复确认后应为 RESTORED，实际 %s", got)
	}

	// 开始前取消：无需收尾，整预约取消。
	r2 := c.createReservation(map[string]twin.Config{"dev-a": {"temp": "10"}}, reservationBase.Add(4*time.Hour), reservationBase.Add(9*time.Hour))
	r2 = c.cancelReservation(r2.ID, "dev-a")
	if r2.Status != twin.ReservationCancelled {
		t.Fatalf("全部设备取消后应为 CANCELLED，实际 %s", r2.Status)
	}
	clock.advance(4 * time.Hour)
	if tv := c.twin("dev-a"); tv.DesiredConfig["temp"] != "18" {
		t.Fatalf("已取消预约不应生效，dev-a 仍应为 18，实际 %v", tv.DesiredConfig)
	}
}

func TestRestoreTargetsCurrentlyValidOperation(t *testing.T) {
	c, clock, _ := newReservationHarness(t)

	c.setDesired("dev-4", twin.Config{"temp": "22"})
	c.confirmEffective("dev-4")

	start := reservationBase.Add(time.Hour)
	r := c.createReservation(map[string]twin.Config{"dev-4": {"temp": "18"}}, start, start.Add(3*time.Hour))
	clock.advance(time.Hour)
	c.confirmEffective("dev-4")

	// 预约生效期间操作者发布新的常规目标 20，取代预约效果。
	c.publishStrategy(map[string]twin.Config{"dev-4": {"temp": "20"}}, twin.ChangeStrategySupersede)
	c.confirmEffective("dev-4")

	// 预约到期：恢复目标参考当前有效操作（20），而不是创建时的旧值 22。
	clock.advance(3 * time.Hour)
	v := c.reservation(r.ID)
	if v.Status != twin.ReservationCompleted {
		t.Fatalf("恢复目标已在位，预约应直接完成，实际 %s", v.Status)
	}
	if tv := c.twin("dev-4"); tv.DesiredConfig["temp"] != "20" {
		t.Fatalf("到期后期望应保持当前有效目标 20，实际 %v", tv.DesiredConfig)
	}
}

func TestReservationStateSurvivesRestart(t *testing.T) {
	c, clock, dbPath := newReservationHarness(t)

	c.setDesired("dev-5", twin.Config{"temp": "22"})
	c.confirmEffective("dev-5")
	c.do(http.MethodPost, "/devices/dev-5/offline", nil, nil)

	start := reservationBase.Add(time.Hour)
	r := c.createReservation(map[string]twin.Config{"dev-5": {"temp": "18"}}, start, start.Add(2*time.Hour))
	clock.advance(time.Hour) // 到点，设备离线

	// 服务重开：仍应说明设备为何等待。
	c2 := reservationClient(t, dbPath, clock)
	v := c2.reservation(r.ID)
	dv := devicePhase(v, "dev-5")
	if dv.Phase != twin.PhaseDue || dv.WaitReason == "" {
		t.Fatalf("重开后应继续说明等待原因，实际 %+v", dv)
	}

	// 设备重连续作：拉取预约目标并确认。
	if cfg := c2.confirmEffective("dev-5"); cfg["temp"] != "18" {
		t.Fatalf("重开后设备应完成预约目标 18，实际 %v", cfg)
	}
	if got := devicePhase(c2.reservation(r.ID), "dev-5").Phase; got != twin.PhaseConfirmed {
		t.Fatalf("重开续作后应为 CONFIRMED，实际 %s", got)
	}

	// 再次重开并推进到期：恢复收尾仍可继续。
	clock.advance(2 * time.Hour)
	c3 := reservationClient(t, dbPath, clock)
	if cfg := c3.confirmEffective("dev-5"); cfg["temp"] != "22" {
		t.Fatalf("重开后到期应恢复 22，实际 %v", cfg)
	}
	if v := c3.reservation(r.ID); v.Status != twin.ReservationCompleted {
		t.Fatalf("重开后恢复确认应为 COMPLETED，实际 %s", v.Status)
	}
}
