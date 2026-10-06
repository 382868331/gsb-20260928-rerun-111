package power

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

// 贪心策略（按优先级排序后各自取最早可行开始）会错失可行安排的场景：
// 高优先级任务占住窗口起点，必须后移才能给窗口紧张的任务让位。
func TestSolveBacktracksWhereGreedyFails(t *testing.T) {
	limit := 10.0
	res := []Reservation{
		{ID: "flex", EarliestStart: at(0), LatestStart: at(360), Duration: 600 * time.Minute / 10, Power: 6, Priority: 3},
		{ID: "tight", EarliestStart: at(0), LatestStart: at(24), Duration: 24 * time.Minute, Power: 6, Priority: 2},
	}
	// flex 时长 60 分钟，窗口 [0,360]；tight 必须落在 [0,48] 内。
	res[0].Duration = 60 * time.Minute

	// 贪心：按优先级顺序各自取最早可行开始。
	greedyStarts := map[string]time.Time{}
	blocks := []Block{}
	order := append([]Reservation(nil), res...)
	sort.Slice(order, func(i, j int) bool { return order[i].Priority > order[j].Priority })
	for _, r := range order {
		placed := false
		for _, cand := range CandidateStarts(r, res, nil) {
			if loadOK(blocks, cand, r.Duration, r.Power, limit) {
				blocks = append(blocks, Block{Start: cand, End: cand.Add(r.Duration), Power: r.Power})
				greedyStarts[r.ID] = cand
				placed = true
				break
			}
		}
		if !placed {
			delete(greedyStarts, r.ID)
		}
	}
	if len(greedyStarts) == 2 {
		t.Fatalf("贪心在此场景不应找到完整可行解: %v", greedyStarts)
	}

	plan := Solve(res, nil, limit)
	if len(plan.Assignments) != 2 {
		t.Fatalf("回溯求解应安排全部两项预约: %+v", plan)
	}
	if got := plan.Assignments["tight"]; !got.Equal(at(0)) {
		t.Fatalf("tight 应在 0 开始, 实际 %v", got)
	}
	if got := plan.Assignments["flex"]; !got.Equal(at(24)) {
		t.Fatalf("flex 应后移到 24, 实际 %v", got)
	}
}

// 小规模穷举核对：对所有候选开始时间做笛卡尔积枚举，
// 比较穷举最优目标与 Solve 结果是否一致。
func TestSolveMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20261006))
	for trial := 0; trial < 200; trial++ {
		n := 2 + rng.Intn(3)
		res := make([]Reservation, n)
		for i := range res {
			earliest := rng.Intn(5) * 30
			latest := earliest + rng.Intn(4)*30
			res[i] = Reservation{
				ID:            string(rune('a' + i)),
				EarliestStart: at(earliest),
				LatestStart:   at(latest),
				Duration:      time.Duration(30*(1+rng.Intn(3))) * time.Minute,
				Power:         float64(1 + rng.Intn(6)),
				Priority:      rng.Intn(4),
			}
		}
		limit := float64(3 + rng.Intn(8))
		var fixed []Block
		if rng.Intn(2) == 0 {
			start := at(rng.Intn(4) * 30)
			fixed = append(fixed, Block{Start: start, End: start.Add(45 * time.Minute), Power: float64(1 + rng.Intn(4)), Ref: "fixed"})
		}
		plan := Solve(res, fixed, limit)
		want := bruteForceBest(res, fixed, limit)
		got := objectiveOf(res, plan)
		if !objectiveEqual(got, want) {
			t.Fatalf("trial %d: 求解结果与穷举不一致 res=%+v fixed=%+v limit=%v got=%v want=%v", trial, res, fixed, limit, got, want)
		}
		// 结果本身必须满足功率约束。
		checkBlocks := append([]Block(nil), fixed...)
		for id, start := range plan.Assignments {
			var r Reservation
			for _, cand := range res {
				if cand.ID == id {
					r = cand
				}
			}
			if !loadOK(checkBlocks, start, r.Duration, r.Power, limit) {
				t.Fatalf("trial %d: 预约 %s 的安排违反功率上限", trial, id)
			}
			checkBlocks = append(checkBlocks, Block{Start: start, End: start.Add(r.Duration), Power: r.Power})
		}
	}
}

type objective struct {
	scheduled []bool
	deviation []time.Duration
}

func objectiveOf(res []Reservation, plan Plan) objective {
	order := priorityOrder(res)
	obj := objective{scheduled: make([]bool, len(order)), deviation: make([]time.Duration, len(order))}
	for i, r := range order {
		start, ok := plan.Assignments[r.ID]
		obj.scheduled[i] = ok
		if ok {
			obj.deviation[i] = start.Sub(r.EarliestStart)
		} else {
			obj.deviation[i] = 1<<62 - 1
		}
	}
	return obj
}

func priorityOrder(res []Reservation) []Reservation {
	order := append([]Reservation(nil), res...)
	sort.Slice(order, func(i, j int) bool {
		if order[i].Priority != order[j].Priority {
			return order[i].Priority > order[j].Priority
		}
		return order[i].ID < order[j].ID
	})
	return order
}

func objectiveEqual(a, b objective) bool {
	if len(a.scheduled) != len(b.scheduled) {
		return false
	}
	for i := range a.scheduled {
		if a.scheduled[i] != b.scheduled[i] || a.deviation[i] != b.deviation[i] {
			return false
		}
	}
	return true
}

func objectiveBetter(a, b objective) bool {
	for i := range a.scheduled {
		if a.scheduled[i] != b.scheduled[i] {
			return a.scheduled[i]
		}
	}
	for i := range a.deviation {
		if !a.scheduled[i] {
			continue
		}
		if a.deviation[i] != b.deviation[i] {
			return a.deviation[i] < b.deviation[i]
		}
	}
	return false
}

// bruteForceBest 枚举每个预约的每个候选开始时间以及“不安排”，返回最优目标。
func bruteForceBest(res []Reservation, fixed []Block, limit float64) objective {
	order := priorityOrder(res)
	cands := make([][]time.Time, len(order))
	for i, r := range order {
		cands[i] = CandidateStarts(r, order, fixed)
	}
	best := objective{scheduled: make([]bool, len(order)), deviation: make([]time.Duration, len(order))}
	for i := range best.deviation {
		best.deviation[i] = 1<<62 - 1
	}
	first := true
	cur := objective{scheduled: make([]bool, len(order)), deviation: make([]time.Duration, len(order))}
	var walk func(idx int, blocks []Block)
	walk = func(idx int, blocks []Block) {
		if idx == len(order) {
			for i := range cur.scheduled {
				if !cur.scheduled[i] {
					cur.deviation[i] = 1<<62 - 1
				}
			}
			if first || objectiveBetter(cur, best) {
				best.scheduled = append([]bool(nil), cur.scheduled...)
				best.deviation = append([]time.Duration(nil), cur.deviation...)
				first = false
			}
			return
		}
		r := order[idx]
		for _, cand := range cands[idx] {
			if !loadOK(blocks, cand, r.Duration, r.Power, limit) {
				continue
			}
			cur.scheduled[idx] = true
			cur.deviation[idx] = cand.Sub(r.EarliestStart)
			walk(idx+1, append(blocks, Block{Start: cand, End: cand.Add(r.Duration), Power: r.Power}))
		}
		cur.scheduled[idx] = false
		cur.deviation[idx] = 1<<62 - 1
		walk(idx+1, blocks)
	}
	walk(0, append([]Block(nil), fixed...))
	return best
}
