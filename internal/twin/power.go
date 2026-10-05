package twin

import (
	"time"
)

// PowerStatus 是功率预约的生命周期状态。
type PowerStatus string

const (
	// PowerStatusPlanned 已算出计划开始时间，尚未到点下发。
	PowerStatusPlanned PowerStatus = "PLANNED"
	// PowerStatusRunning 目标已下发，任务正在连续运行。
	PowerStatusRunning PowerStatus = "RUNNING"
	// PowerStatusStopping 停止目标已发出但设备尚未确认，容量仍被占用。
	PowerStatusStopping PowerStatus = "STOPPING"
	// PowerStatusDone 停止已被设备确认，容量真正释放。
	PowerStatusDone PowerStatus = "DONE"
	// PowerStatusUnschedulable 当前条件下无法安排，冲突原因已记录。
	PowerStatusUnschedulable PowerStatus = "UNSCHEDULABLE"
	// PowerStatusCancelled 未开始前被取消。
	PowerStatusCancelled PowerStatus = "CANCELLED"
)

// PowerConflictView 是无法安排时给出的真实冲突预约与超限区间。
type PowerConflictView struct {
	ReservationIDs []string            `json:"reservation_ids"`
	Overloads      []PowerOverloadView `json:"overloads"`
	Reason         string              `json:"reason,omitempty"`
}

// PowerOverloadView 是一个超限区间。
type PowerOverloadView struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	RequiredKW float64   `json:"required_kw"`
	LimitKW    float64   `json:"limit_kw"`
	SourceIDs  []string  `json:"source_ids"`
}

// PowerReservation 声明一个需要站点功率容量的连续运行任务。
type PowerReservation struct {
	ID              string             `json:"id"`
	DeviceID        string             `json:"device_id"`
	Target          Config             `json:"target"`
	WindowStart     time.Time          `json:"window_start"`
	WindowEnd       time.Time          `json:"window_end"`
	DurationSeconds int64              `json:"duration_seconds"`
	PowerKW         float64            `json:"power_kw"`
	Priority        int                `json:"priority"`
	Emergency       bool               `json:"emergency,omitempty"`
	AllowInterrupt  bool               `json:"allow_interrupt,omitempty"`
	Status          PowerStatus        `json:"status"`
	PlannedStart    *time.Time         `json:"planned_start,omitempty"`
	PlannedEnd      *time.Time         `json:"planned_end,omitempty"`
	StartedAt       *time.Time         `json:"started_at,omitempty"`
	FinishedAt      *time.Time         `json:"finished_at,omitempty"`
	Interrupted     bool               `json:"interrupted,omitempty"`
	ChangeID        string             `json:"change_id,omitempty"`
	Conflict        *PowerConflictView `json:"conflict,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// PowerReservationRequest 是创建功率预约的输入。
type PowerReservationRequest struct {
	DeviceID        string    `json:"device_id"`
	Target          Config    `json:"target"`
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	DurationSeconds int64     `json:"duration_seconds"`
	PowerKW         float64   `json:"power_kw"`
	Priority        int       `json:"priority"`
	Emergency       bool      `json:"emergency"`
	AllowInterrupt  bool      `json:"allow_interrupt"`
}

// PowerPlanView 是当前计划与已承诺负载曲线的只读视图。
type PowerPlanView struct {
	PowerLimitKW float64             `json:"power_limit_kw"`
	LimitSet     bool                `json:"limit_set"`
	Reservations []*PowerReservation `json:"reservations"`
	Load         []PowerLoadSegment  `json:"load"`
	Overloads    []PowerOverloadView `json:"overloads,omitempty"`
}

// PowerLoadSegment 是已承诺负载曲线的一段。
type PowerLoadSegment struct {
	Start        time.Time  `json:"start"`
	End          *time.Time `json:"end,omitempty"`
	PowerKW      float64    `json:"power_kw"`
	Reservations []string   `json:"reservations"`
}

const emergencyPriorityBoost = 1 << 30

func (r *PowerReservation) effectivePriority() int {
	if r.Emergency {
		return r.Priority + emergencyPriorityBoost
	}
	return r.Priority
}

func (r *PowerReservation) clone() *PowerReservation {
	cp := *r
	cp.Target = cloneConfig(r.Target)
	if r.Conflict != nil {
		c := *r.Conflict
		c.ReservationIDs = append([]string(nil), r.Conflict.ReservationIDs...)
		c.Overloads = append([]PowerOverloadView(nil), r.Conflict.Overloads...)
		cp.Conflict = &c
	}
	return &cp
}
