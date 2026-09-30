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
	// StatusSuperseded 只用于变更记录：该版本尚未全部生效就被更新的目标替代。
	StatusSuperseded Status = "SUPERSEDED"
)

// ChangeKind 区分普通目标与失败后发起的补偿目标。
type ChangeKind string

const (
	ChangeNormal       ChangeKind = "NORMAL"
	ChangeCompensation ChangeKind = "COMPENSATION"
)

// Change 是一次期望发布在单台设备上留下的不可丢弃记录。
// 即使版本从未下发或后来被替代，记录仍保留，用于离线连改、重启后的核对与审计。
type Change struct {
	Rev         int64      `json:"rev"`
	TargetID    string     `json:"target_id"`
	Kind        ChangeKind `json:"kind"`
	Config      Config     `json:"config"`
	Status      Status     `json:"status"`
	Rejected    Config     `json:"rejected,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// Device 保存一台设备的孪生三态：期望、已上报、已确认。
type Device struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
	// DesiredConfig 是服务端最新目标（整份快照），DesiredRev 为单调递增版本号。
	DesiredConfig Config `json:"desired_config"`
	DesiredRev    int64  `json:"desired_rev"`
	// DeliveredRev 是最近一次设备拉取过的期望版本；仅表示已下发。
	DeliveredRev int64     `json:"delivered_rev"`
	DeliveredAt  time.Time `json:"delivered_at"`
	// ReportedConfig 是设备最近上报的运行配置，ReportedRev 为上报所针对的期望版本。
	ReportedConfig Config    `json:"reported_config"`
	ReportedRev    int64     `json:"reported_rev"`
	ReportedAt     time.Time `json:"reported_at"`
	// ConfirmedConfig 是服务端确认与期望一致的实际状态，旧确认不会被迟到上报回退。
	ConfirmedConfig Config    `json:"confirmed_config"`
	ConfirmedRev    int64     `json:"confirmed_rev"`
	ConfirmedAt     time.Time `json:"confirmed_at"`
	// Changes 保留该设备每一次期望发布的记录，包括已被替代、从未下发完成的版本。
	Changes []*Change `json:"changes"`
}

// Status 计算当前期望版本所处状态。
func (d *Device) Status(now time.Time) Status {
	if len(d.DesiredConfig) == 0 {
		return StatusPending
	}
	if d.ConfirmedRev >= d.DesiredRev && configEqual(d.ConfirmedConfig, d.DesiredConfig) {
		return StatusEffective
	}
	if d.ConfirmedRev >= d.DesiredRev {
		// 版本号到了但配置不一致：设备拒绝了部分配置。
		return StatusPartial
	}
	if d.DeliveredRev >= d.DesiredRev && !d.DeliveredAt.IsZero() {
		return StatusDelivered
	}
	return StatusPending
}

type ConflictReason string

const (
	// ReasonRejected 设备明确拒绝部分配置项。
	ReasonRejected ConflictReason = "REJECTED"
	// ReasonStale 迟到上报：上报版本低于当前期望版本，已被忽略，不能盖掉最新目标。
	ReasonStale ConflictReason = "STALE_REPORT"
	// ReasonSuperseded 离线期间期望被连续修改，一个尚未完成的下发版本被跳过。
	ReasonSuperseded ConflictReason = "SUPERSEDED"
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

// Target 是一次发布：可能包含一组设备上的相关配置，必须每台设备各自确认后才算整体生效。
type Target struct {
	ID          string       `json:"id"`
	Kind        ChangeKind   `json:"kind"`
	Compensates string       `json:"compensates,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	Items       []TargetItem `json:"items"`
}

// TargetItem 记录目标在某台设备上对应哪个期望版本。
type TargetItem struct {
	DeviceID string `json:"device_id"`
	Rev      int64  `json:"rev"`
}

// TargetItemView 是目标中一台设备的实时生效情况，只取该设备自己的孪生状态。
type TargetItemView struct {
	DeviceID        string `json:"device_id"`
	Rev             int64  `json:"rev"`
	Status          Status `json:"status"`
	StatusText      string `json:"status_text"`
	DesiredRev      int64  `json:"desired_rev"`
	ReportedRev     int64  `json:"reported_rev"`
	ConfirmedRev    int64  `json:"confirmed_rev"`
	ConfirmedConfig Config `json:"confirmed_config"`
}

// TargetView 展示目标整体生效条件：任何一台设备仅已下发或部分生效，整体都不算生效。
type TargetView struct {
	ID          string           `json:"id"`
	Kind        ChangeKind       `json:"kind"`
	Compensates string           `json:"compensates,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	Overall     Status           `json:"overall_status"`
	OverallText string           `json:"overall_status_text"`
	Items       []TargetItemView `json:"items"`
}

// FieldDiff 描述单个配置项的新旧值。
type FieldDiff struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ConfigDiff 是新目标发布前的预演差异，不产生任何状态变更。
type ConfigDiff struct {
	Add    Config               `json:"add"`
	Remove Config               `json:"remove"`
	Change map[string]FieldDiff `json:"change"`
}

// DiffConfig 对比两份整份配置快照。
func DiffConfig(base, next Config) ConfigDiff {
	diff := ConfigDiff{Add: Config{}, Remove: Config{}, Change: map[string]FieldDiff{}}
	for k, v := range next {
		old, ok := base[k]
		switch {
		case !ok:
			diff.Add[k] = v
		case old != v:
			diff.Change[k] = FieldDiff{From: old, To: v}
		}
	}
	for k, v := range base {
		if _, ok := next[k]; !ok {
			diff.Remove[k] = v
		}
	}
	return diff
}

// ReportRequest 是设备上报的配置结果。
type ReportRequest struct {
	Rev      int64  `json:"rev"`
	Applied  Config `json:"applied"`
	Rejected Config `json:"rejected"`
}

// TwinView 是对外展示的完整孪生视图，三态严格区分。
type TwinView struct {
	DeviceID        string `json:"device_id"`
	Online          bool   `json:"online"`
	Status          Status `json:"status"`
	StatusText      string `json:"status_text"`
	DesiredRev      int64  `json:"desired_rev"`
	DesiredConfig   Config `json:"desired_config"`
	DeliveredRev    int64  `json:"delivered_rev"`
	ReportedRev     int64  `json:"reported_rev"`
	ReportedConfig  Config `json:"reported_config"`
	ConfirmedRev    int64  `json:"confirmed_rev"`
	ConfirmedConfig Config `json:"confirmed_config"`
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
