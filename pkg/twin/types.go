package twin

import "time"

// 配置项状态：pending 待下发 / delivered 已下发未确认 / effective 已生效 / rejected 设备拒绝
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusEffective = "effective"
	StatusRejected  = "rejected"
)

// 冲突类型：stale_ack 迟到的旧版本确认 / rejected 设备拒绝 / diverged 上报值与目标不符
const (
	ConflictStaleAck = "stale_ack"
	ConflictRejected = "rejected"
	ConflictDiverged = "diverged"
)

type DesiredState struct {
	Value     string    `json:"value"`
	Version   int64     `json:"version"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ReportedState struct {
	Value   string    `json:"value"`
	At      time.Time `json:"at"`
	Version int64     `json:"version,omitempty"`
}

type ConfirmedState struct {
	Value   string    `json:"value"`
	Version int64     `json:"version"`
	At      time.Time `json:"at"`
}

type Conflict struct {
	At              time.Time `json:"at"`
	Key             string    `json:"key"`
	Kind            string    `json:"kind"`
	Reason          string    `json:"reason,omitempty"`
	DesiredValue    string    `json:"desired_value,omitempty"`
	DesiredVersion  int64     `json:"desired_version,omitempty"`
	ReportedValue   string    `json:"reported_value,omitempty"`
	ReportedVersion int64     `json:"reported_version,omitempty"`
}

type DeviceTwin struct {
	ID        string                    `json:"id"`
	Desired   map[string]DesiredState   `json:"desired"`
	Reported  map[string]ReportedState  `json:"reported"`
	Confirmed map[string]ConfirmedState `json:"confirmed"`
	Conflicts []Conflict                `json:"conflicts"`
}

func newDeviceTwin(id string) *DeviceTwin {
	return &DeviceTwin{
		ID:        id,
		Desired:   map[string]DesiredState{},
		Reported:  map[string]ReportedState{},
		Confirmed: map[string]ConfirmedState{},
	}
}

// ---- HTTP 请求/响应结构 ----

type SetDesiredRequest struct {
	Config map[string]string `json:"config"`
}

type AppliedItem struct {
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type PollRequest struct {
	Applied map[string]AppliedItem `json:"applied"`
}

type DesiredItem struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type PollResponse struct {
	DeviceID string        `json:"device_id"`
	Config   []DesiredItem `json:"config"`
}

type ReportItem struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"`
}

type ReportRequest struct {
	Items    []ReportItem      `json:"items,omitempty"`
	Snapshot map[string]string `json:"snapshot,omitempty"`
}
