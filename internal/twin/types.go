package twin

import "time"

// 配置项是字符串键值，足以表达温度、模式等简单参数。
type Config map[string]string

// Status 是期望配置相对设备的生命周期状态。
type Status string

const (
	// StatusPending 有期望目标，但尚未确认下发到设备。
	StatusPending Status = "PENDING"
	// StatusDelivered 配置已下发给设备，但尚未收到生效确认。不能视为已生效。
	StatusDelivered Status = "DELIVERED"
	// StatusPartial 设备拒绝了部分配置，只有一部分已确认生效。
	StatusPartial Status = "PARTIAL"
	// StatusEffective 设备已确认期望配置全部生效。
	StatusEffective Status = "EFFECTIVE"
	// StatusFailed 设备未能应用目标，需要由新的目标或补偿目标接管。
	StatusFailed Status = "FAILED"
	// StatusSuperseded 目标尚未整体完成就被新目标替代，只保留历史。
	StatusSuperseded Status = "SUPERSEDED"
	// StatusQueued 目标正在等待同一设备先前操作结束，尚未生成期望版本。
	StatusQueued Status = "QUEUED"
)

type ChangeKind string

const (
	ChangeKindTarget       ChangeKind = "TARGET"
	ChangeKindCompensation ChangeKind = "COMPENSATION"
	// ChangeKindReservation 预约到点时发布的生效变更。
	ChangeKindReservation ChangeKind = "RESERVATION"
	// ChangeKindRestore 预约到期或取消后发布的恢复变更。
	ChangeKindRestore ChangeKind = "RESTORE"
)

type ChangeStrategy string

const (
	// ChangeStrategyQueue 不抢占设备尚未结束的操作，按发布顺序等待。
	ChangeStrategyQueue ChangeStrategy = "QUEUE"
	// ChangeStrategySupersede 显式取代设备尚未结束的操作，立即发布新目标。
	ChangeStrategySupersede ChangeStrategy = "SUPERSEDE"
)

// Change 是一次面向一组设备整体发布的配置目标。
// 只有 Targets 中每台设备都精确确认自己的目标，整个变更才生效。
type Change struct {
	ID              string                        `json:"id"`
	Kind            ChangeKind                    `json:"kind"`
	Targets         map[string]Config             `json:"targets"`
	BaselineTargets map[string]Config             `json:"baseline_targets,omitempty"`
	DeliveredRevs   map[string]int64              `json:"delivered_revs"`
	DeviceResults   map[string]ChangeDeviceResult `json:"device_results"`
	Status          Status                        `json:"status"`
	Strategy        ChangeStrategy                `json:"strategy"`
	ReadyDevices    []string                      `json:"ready_devices,omitempty"`
	WaitingDevices  []string                      `json:"waiting_devices,omitempty"`
	Supersedes      []string                      `json:"supersedes,omitempty"`
	CompensatesFor  string                        `json:"compensates_for,omitempty"`
	ReservationID   string                        `json:"reservation_id,omitempty"`
	CreatedAt       time.Time                     `json:"created_at"`
	EffectiveAt     *time.Time                    `json:"effective_at,omitempty"`
	SupersededAt    *time.Time                    `json:"superseded_at,omitempty"`
}

// Reservation 是一次带有效期的预约变更：StartAt 起生效，EndAt 到期后恢复到当时仍有效的目标。
type Reservation struct {
	ID               string            `json:"id"`
	Targets          map[string]Config `json:"targets"`
	StartAt          time.Time         `json:"start_at"`
	EndAt            time.Time         `json:"end_at"`
	ActivateChangeID string            `json:"activate_change_id,omitempty"`
	// RestoreChangeIDs 记录每台设备的收尾恢复变更；键存在即表示已安排收尾，空值表示无需变更。
	RestoreChangeIDs map[string]string `json:"restore_change_ids,omitempty"`
	CancelledDevices map[string]bool   `json:"cancelled_devices,omitempty"`
	Expired          bool              `json:"expired"`
	CreatedAt        time.Time         `json:"created_at"`
}

// ReservationPhase 是单台设备在一次预约中的进度，严格区分“应当生效”与“已经确认生效”。
type ReservationPhase string

const (
	// PhaseWaitingStart 未到预约开始时间。
	PhaseWaitingStart ReservationPhase = "WAITING_START"
	// PhaseDue 开始时间已到，预约目标应当生效，但设备尚未确认。
	PhaseDue ReservationPhase = "DUE"
	// PhaseConfirmed 设备已精确确认预约目标生效。
	PhaseConfirmed ReservationPhase = "CONFIRMED"
	// PhaseRestoreDue 预约已到期/被取消，恢复目标已安排，仍等待设备核实。
	PhaseRestoreDue ReservationPhase = "RESTORE_DUE"
	// PhaseRestored 设备已确认恢复到当时仍有效的目标。
	PhaseRestored ReservationPhase = "RESTORED"
	// PhaseCancelled 设备被操作者取消，无需收尾。
	PhaseCancelled ReservationPhase = "CANCELLED"
	// PhaseSkipped 预约在生效前就已到期，从未执行。
	PhaseSkipped ReservationPhase = "SKIPPED"
)

type ReservationStatus string

const (
	ReservationScheduled ReservationStatus = "SCHEDULED"
	ReservationActive    ReservationStatus = "ACTIVE"
	ReservationRestoring ReservationStatus = "RESTORING"
	ReservationCompleted ReservationStatus = "COMPLETED"
	ReservationCancelled ReservationStatus = "CANCELLED"
)

// ReservationDeviceView 说明一台设备在预约中的进度与等待原因。
type ReservationDeviceView struct {
	DeviceID         string           `json:"device_id"`
	Online           bool             `json:"online"`
	Phase            ReservationPhase `json:"phase"`
	PhaseText        string           `json:"phase_text"`
	WaitReason       string           `json:"wait_reason,omitempty"`
	ActivateChangeID string           `json:"activate_change_id,omitempty"`
	RestoreChangeID  string           `json:"restore_change_id,omitempty"`
}

// ReservationView 是预约的对外视图，整体状态由逐设备进度推导。
type ReservationView struct {
	*Reservation
	Status     ReservationStatus       `json:"status"`
	StatusText string                  `json:"status_text"`
	Devices    []ReservationDeviceView `json:"devices"`
}

// configLayer 是设备上的一层有效配置来源：底层是普通变更，上层按激活顺序叠放预约。
// 预约到期时移除本层，剩余最上层即为“当时仍然有效的目标”。
type configLayer struct {
	ChangeID      string `json:"change_id"`
	ReservationID string `json:"reservation_id,omitempty"`
	Config        Config `json:"config"`
}

type ChangeDeviceResult struct {
	DesiredRev    int64     `json:"desired_rev"`
	DeliveredRev  int64     `json:"delivered_rev"`
	ReportedRev   int64     `json:"reported_rev"`
	AppliedConfig Config    `json:"applied_config"`
	Rejected      Config    `json:"rejected,omitempty"`
	Status        Status    `json:"status"`
	DispatchState string    `json:"dispatch_state,omitempty"`
	WaitForChange string    `json:"wait_for_change,omitempty"`
	Stale         bool      `json:"stale,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Device 保存一台设备的孪生三态：期望、已上报、已确认。
type Device struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
	// DesiredConfig 是服务端最新目标（整份快照），DesiredRev 为单调递增版本号。
	DesiredConfig   Config `json:"desired_config"`
	DesiredRev      int64  `json:"desired_rev"`
	DesiredChangeID string `json:"desired_change_id,omitempty"`
	// DeliveredRev 是最近一次设备拉取过的期望版本；仅表示已下发。
	DeliveredRev int64     `json:"delivered_rev"`
	DeliveredAt  time.Time `json:"delivered_at"`
	// ReportedConfig 是设备最近上报的运行配置，ReportedRev 为上报所针对的期望版本。
	ReportedConfig   Config    `json:"reported_config"`
	ReportedRev      int64     `json:"reported_rev"`
	ReportedRejected Config    `json:"reported_rejected,omitempty"`
	ReportedAt       time.Time `json:"reported_at"`
	// ConfirmedConfig 是服务端确认与期望一致的实际状态，旧确认不会被迟到上报回退。
	ConfirmedConfig Config    `json:"confirmed_config"`
	ConfirmedRev    int64     `json:"confirmed_rev"`
	ConfirmedAt     time.Time `json:"confirmed_at"`
}

// Status 计算当前期望版本所处状态。
func (d *Device) Status(now time.Time) Status {
	if len(d.DesiredConfig) == 0 {
		return StatusPending
	}
	if d.DesiredRev > 0 && d.ConfirmedRev >= d.DesiredRev && configEqual(d.ConfirmedConfig, d.DesiredConfig) {
		return StatusEffective
	}
	if d.ReportedRev == d.DesiredRev && !d.ReportedAt.IsZero() {
		if len(d.ReportedRejected) > 0 {
			return StatusPartial
		}
		return StatusFailed
	}
	if d.DeliveredRev >= d.DesiredRev && !d.DeliveredAt.IsZero() {
		return StatusDelivered
	}
	return StatusPending
}

func (d *Device) isDesiredConfirmedLocked() bool {
	return d.DesiredRev > 0 && d.ConfirmedRev >= d.DesiredRev && configEqual(d.ConfirmedConfig, d.DesiredConfig)
}

type ConflictReason string

const (
	// ReasonRejected 设备明确拒绝部分配置项。
	ReasonRejected ConflictReason = "REJECTED"
	// ReasonStale 迟到上报：上报版本低于当前期望版本，已被忽略，不能盖掉最新目标。
	ReasonStale ConflictReason = "STALE_REPORT"
	// ReasonSuperseded 离线期间期望被连续修改，一个尚未完成的下发版本被跳过。
	ReasonSuperseded ConflictReason = "SUPERSEDED"
	// ReasonMismatch 设备针对当前版本上报的实际配置与期望既不完全一致，也没有逐项列明拒绝。
	ReasonMismatch ConflictReason = "APPLIED_MISMATCH"
)

// Conflict 记录期望与设备实际之间出现过的分歧。
type Conflict struct {
	ID         string         `json:"id"`
	DeviceID   string         `json:"device_id"`
	Reason     ConflictReason `json:"reason"`
	Rev        int64          `json:"rev"`
	Detail     Config         `json:"detail"`
	Open       bool           `json:"open"`
	CreatedAt  time.Time      `json:"created_at"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty"`
}

// ReportRequest 是设备上报的配置结果。
type ReportRequest struct {
	Rev      int64  `json:"rev"`
	Applied  Config `json:"applied"`
	Rejected Config `json:"rejected"`
}

type DeviceDiff struct {
	DeviceID      string   `json:"device_id"`
	CurrentRev    int64    `json:"current_rev"`
	CurrentConfig Config   `json:"current_config"`
	TargetConfig  Config   `json:"target_config"`
	Added         Config   `json:"added"`
	Updated       Config   `json:"updated"`
	Removed       []string `json:"removed"`
	ConfirmedRev  int64    `json:"confirmed_rev"`
	ReportedRev   int64    `json:"reported_rev"`
	CurrentStatus Status   `json:"current_status"`
	Ready         bool     `json:"ready"`
	WaitForChange string   `json:"wait_for_change,omitempty"`
}

type Preview struct {
	Targets map[string]Config `json:"targets"`
	Diffs   []DeviceDiff      `json:"diffs"`
}

type ChangeView struct {
	*Change
	StatusText string `json:"status_text"`
}

// TwinView 是对外展示的完整孪生视图，三态严格区分。
type TwinView struct {
	DeviceID         string `json:"device_id"`
	Online           bool   `json:"online"`
	DesiredChangeID  string `json:"desired_change_id,omitempty"`
	Status           Status `json:"status"`
	StatusText       string `json:"status_text"`
	DesiredRev       int64  `json:"desired_rev"`
	DesiredConfig    Config `json:"desired_config"`
	DeliveredRev     int64  `json:"delivered_rev"`
	ReportedRev      int64  `json:"reported_rev"`
	ReportedConfig   Config `json:"reported_config"`
	ReportedRejected Config `json:"reported_rejected,omitempty"`
	ConfirmedRev     int64  `json:"confirmed_rev"`
	ConfirmedConfig  Config `json:"confirmed_config"`
}

func configEqual(a, b Config) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
