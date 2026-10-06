// Package power 实现站点级功率约束下的预约调度求解。
// 每项预约声明允许开始窗口、连续运行时长、运行功率与优先级；
// 求解器选择实际开始时间，使任一时刻站点总功率不超过上限。
// 调度目标：先按优先级尽量满足更多预约，再尽量减少启动偏移。
// 求解只移动开始时间，绝不缩短任务时长。
package power

import (
	"sort"
	"time"
)

// Reservation 是一项待调度的功率预约。
type Reservation struct {
	ID            string
	EarliestStart time.Time
	LatestStart   time.Time
	Duration      time.Duration
	Power         float64
	Priority      int
}

// Block 是一段已确定占用功率的区间（运行中或已派发的任务）。
type Block struct {
	Start time.Time
	End   time.Time
	Power float64
	Ref   string
}

// Interval 是一段超限区间，说明哪些时段功率需求超过上限。
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Load  float64   `json:"load"`
	Limit float64   `json:"limit"`
}

// Unscheduled 记录一项无法安排的预约及其实际冲突来源。
type Unscheduled struct {
	ReservationID string     `json:"reservation_id"`
	Reason        string     `json:"reason"`
	ConflictsWith []string   `json:"conflicts_with"`
	Overloads     []Interval `json:"overloads"`
}

// Plan 是求解结果：可行预约的开始时间，以及不可行预约的冲突说明。
type Plan struct {
	Assignments map[string]time.Time `json:"assignments"`
	Unscheduled []Unscheduled        `json:"unscheduled"`
}

// CandidateStarts 计算一项预约值得尝试的开始时刻集合：
// 自身最早开始、所有预约窗口端点与时长端点、固定区间的端点，
// 落在本预约窗口内的点。最优解一定存在于此集合中。
func CandidateStarts(r Reservation, all []Reservation, fixed []Block) []time.Time {
	set := map[time.Time]bool{}
	add := func(t time.Time) {
		t = t.UTC()
		if !t.Before(r.EarliestStart) && !t.After(r.LatestStart) {
			set[t] = true
		}
	}
	add(r.EarliestStart)
	for _, other := range all {
		add(other.EarliestStart)
		add(other.EarliestStart.Add(other.Duration))
		add(other.LatestStart)
	}
	for _, b := range fixed {
		add(b.Start)
		add(b.End)
	}
	out := make([]time.Time, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// loadOK 检查在已有区间基础上加入 [start, start+dur) 后是否全程不超限。
func loadOK(blocks []Block, start time.Time, dur time.Duration, power, limit float64) bool {
	cand := Block{Start: start, End: start.Add(dur), Power: power}
	points := map[time.Time]bool{start: true}
	for _, b := range blocks {
		if b.End.After(cand.Start) && b.Start.Before(cand.End) {
			if !b.Start.Before(cand.Start) {
				points[b.Start] = true
			}
		}
	}
	for t := range points {
		total := power
		for _, b := range blocks {
			if !b.Start.After(t) && b.End.After(t) {
				total += b.Power
			}
		}
		if total > limit+1e-9 {
			return false
		}
	}
	return true
}

// overloadIntervals 计算预约窗口内剩余容量不足（<need）的区间及当时的负载。
func overloadIntervals(r Reservation, blocks []Block, limit float64) []Interval {
	winStart := r.EarliestStart
	winEnd := r.LatestStart.Add(r.Duration)
	points := map[time.Time]bool{winStart: true}
	for _, b := range blocks {
		if b.End.After(winStart) && b.Start.Before(winEnd) {
			if !b.Start.Before(winStart) && b.Start.Before(winEnd) {
				points[b.Start] = true
			}
			if b.End.After(winStart) && b.End.Before(winEnd) {
				points[b.End] = true
			}
		}
	}
	ts := make([]time.Time, 0, len(points))
	for t := range points {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	out := []Interval{}
	for i, t := range ts {
		end := winEnd
		if i+1 < len(ts) {
			end = ts[i+1]
		}
		if !end.After(t) {
			continue
		}
		load := 0.0
		for _, b := range blocks {
			if !b.Start.After(t) && b.End.After(t) {
				load += b.Power
			}
		}
		if limit-load < r.Power-1e-9 {
			out = append(out, Interval{Start: t, End: end, Load: load, Limit: limit})
		}
	}
	return out
}

type searchState struct {
	scheduled []bool
	starts    []time.Time
}

// Solve 用回溯搜索求解。预约按（优先级降序、ID 升序）固定排序，
// 目标按该顺序字典序最大化“是否被安排”，再字典序最小化启动偏移。
// fixed 是已经开始或已派发、不可再移动的任务区间。
func Solve(res []Reservation, fixed []Block, limit float64) Plan {
	order := append([]Reservation(nil), res...)
	sort.Slice(order, func(i, j int) bool {
		if order[i].Priority != order[j].Priority {
			return order[i].Priority > order[j].Priority
		}
		return order[i].ID < order[j].ID
	})
	cands := make([][]time.Time, len(order))
	for i, r := range order {
		cands[i] = CandidateStarts(r, order, fixed)
	}
	better := func(cur, best *searchState) bool {
		if best == nil {
			return true
		}
		for i := range order {
			if cur.scheduled[i] != best.scheduled[i] {
				return cur.scheduled[i]
			}
		}
		for i := range order {
			if !cur.scheduled[i] {
				continue
			}
			curDev := cur.starts[i].Sub(order[i].EarliestStart)
			bestDev := best.starts[i].Sub(order[i].EarliestStart)
			if curDev != bestDev {
				return curDev < bestDev
			}
		}
		return false
	}
	blocks := append([]Block(nil), fixed...)
	cur := &searchState{scheduled: make([]bool, len(order)), starts: make([]time.Time, len(order))}
	var best *searchState
	var dfs func(idx int)
	dfs = func(idx int) {
		if idx == len(order) {
			if better(cur, best) {
				best = &searchState{
					scheduled: append([]bool(nil), cur.scheduled...),
					starts:    append([]time.Time(nil), cur.starts...),
				}
			}
			return
		}
		r := order[idx]
		for _, t := range cands[idx] {
			if !loadOK(blocks, t, r.Duration, r.Power, limit) {
				continue
			}
			cur.scheduled[idx] = true
			cur.starts[idx] = t
			blocks = append(blocks, Block{Start: t, End: t.Add(r.Duration), Power: r.Power, Ref: r.ID})
			dfs(idx + 1)
			blocks = blocks[:len(blocks)-1]
			cur.scheduled[idx] = false
		}
		// 也尝试不安排该预约（可能整体更优或根本不可行）。
		cur.scheduled[idx] = false
		dfs(idx + 1)
	}
	dfs(0)
	plan := Plan{Assignments: map[string]time.Time{}, Unscheduled: []Unscheduled{}}
	assignedBlocks := append([]Block(nil), fixed...)
	for i, r := range order {
		if best != nil && best.scheduled[i] {
			plan.Assignments[r.ID] = best.starts[i]
			assignedBlocks = append(assignedBlocks, Block{Start: best.starts[i], End: best.starts[i].Add(r.Duration), Power: r.Power, Ref: r.ID})
		}
	}
	for i, r := range order {
		if best != nil && best.scheduled[i] {
			continue
		}
		// 冲突说明：在已确定（更高优先级）区间背景下，窗口内哪些时段容量不足。
		others := []Block{}
		for _, b := range assignedBlocks {
			if b.Ref != r.ID {
				others = append(others, b)
			}
		}
		overloads := overloadIntervals(r, others, limit)
		conflicts := map[string]bool{}
		for _, b := range others {
			if b.Ref == "" {
				continue
			}
			if b.End.After(r.EarliestStart) && b.Start.Before(r.LatestStart.Add(r.Duration)) {
				conflicts[b.Ref] = true
			}
		}
		conflictIDs := make([]string, 0, len(conflicts))
		for id := range conflicts {
			conflictIDs = append(conflictIDs, id)
		}
		sort.Strings(conflictIDs)
		plan.Unscheduled = append(plan.Unscheduled, Unscheduled{
			ReservationID: r.ID,
			Reason:        "站点功率上限在窗口内不足，无法在不缩短任务的前提下安排",
			ConflictsWith: conflictIDs,
			Overloads:     overloads,
		})
	}
	return plan
}
