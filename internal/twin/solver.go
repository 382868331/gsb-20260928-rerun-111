package twin

import (
	"math"
	"sort"
)

// 站点级功率调度求解器：输入不可移动的固定占用（已启动任务）与一组待安排预约，
// 输出每个预约的实际开始时间。任一时刻站点总功率不得超过上限，任务时长绝不截断。

const infiniteEnd = math.MaxInt64 / 4

type solveInterval struct {
	start  int64
	end    int64
	power  float64
	device string
	id     string
}

type solveTask struct {
	id      string
	device  string
	ws      int64 // 允许开始窗口起点（含）
	we      int64 // 允许开始窗口终点（含，最晚开始）
	dur     int64 // 必须连续运行的时长（秒）
	power   float64
	prio    int
	keep    int64 // 当前计划开始时间，重排时尽量不动不受影响任务
	hasKeep bool
}

// OverloadInterval 描述一个无论如何放置都会超限的区间。
type OverloadInterval struct {
	Start      int64
	End        int64
	RequiredKW float64
	LimitKW    float64
	SourceIDs  []string
}

// DroppedTask 是未能安排的预约及其真实冲突原因。
type DroppedTask struct {
	ID        string
	Conflicts []string
	Overloads []OverloadInterval
}

type solveResult struct {
	placement map[string]int64
	dropped   []DroppedTask
}

type planScore struct {
	sat      []int   // 每个优先级级别（从高到低）已满足的数量
	offset   int64   // 所有已安排任务的启动偏移之和
	keepMiss int     // 与原有计划开始时间不一致的任务数
	startSum int64   // 开始时间总和，用于确定性决胜
	starts   []int64 // 按任务顺序的开始时间，-1 表示未安排
}

func betterScore(a, b *planScore) bool {
	for i := range a.sat {
		if a.sat[i] != b.sat[i] {
			return a.sat[i] > b.sat[i]
		}
	}
	if a.offset != b.offset {
		return a.offset < b.offset
	}
	if a.keepMiss != b.keepMiss {
		return a.keepMiss < b.keepMiss
	}
	if a.startSum != b.startSum {
		return a.startSum < b.startSum
	}
	for i := range a.starts {
		if a.starts[i] != b.starts[i] {
			return a.starts[i] < b.starts[i]
		}
	}
	return false
}

// intervalFits 检查候选区间能否加入已占用集合：
// 同一设备不能重叠，且任一时刻功率之和不得超过上限。
func intervalFits(placed []solveInterval, iv solveInterval, cap float64) bool {
	pts := []int64{iv.start, iv.end}
	for _, p := range placed {
		if p.device == iv.device && p.start < iv.end && iv.start < p.end {
			return false
		}
		if p.start < iv.end && iv.start < p.end {
			pts = append(pts, max64(p.start, iv.start), min64(p.end, iv.end))
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	pts = uniqueInts(pts)
	for i := 0; i+1 < len(pts); i++ {
		at := pts[i]
		if pts[i+1] <= at {
			continue
		}
		load := iv.power
		for _, p := range placed {
			if p.start <= at && at < p.end {
				load += p.power
			}
		}
		if load > cap+1e-9 {
			return false
		}
	}
	return true
}

// closureStarts 计算候选开始时间的闭包：窗口起点、固定占用结束边界，
// 以及这些点加上任意任务时长得到的时间（左移最优调度中每个任务要么在
// 窗口起点开始，要么紧跟另一个任务或固定占用的结束）。
func closureStarts(tasks []solveTask, fixed []solveInterval) []int64 {
	maxWe := int64(0)
	for _, t := range tasks {
		if t.we > maxWe {
			maxWe = t.we
		}
	}
	set := map[int64]bool{}
	for _, t := range tasks {
		set[t.ws] = true
	}
	for _, f := range fixed {
		if f.end < infiniteEnd && f.end <= maxWe {
			set[f.end] = true
		}
	}
	for {
		added := false
		for base := range set {
			for _, t := range tasks {
				c := base + t.dur
				if c <= maxWe && !set[c] {
					set[c] = true
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	out := make([]int64, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// candidateStarts 从闭包中过滤出落在允许开始窗口内的时间，并保留原计划时间。
func candidateStarts(t solveTask, closure []int64) []int64 {
	out := []int64{}
	for _, c := range closure {
		if c >= t.ws && c <= t.we {
			out = append(out, c)
		}
	}
	if t.hasKeep && t.keep >= t.ws && t.keep <= t.we && !containsInt64(out, t.keep) {
		out = append(out, t.keep)
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	}
	return out
}

func containsInt64(values []int64, want int64) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// solvePowerPlan 用分支限界搜索最优安排：先按优先级逐级最大化满足数量，
// 再最小化启动偏移，再尽量保持原计划不变，最后确定性决胜。
func solvePowerPlan(tasks []solveTask, fixed []solveInterval, cap float64) solveResult {
	order := make([]int, len(tasks))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ta, tb := tasks[order[a]], tasks[order[b]]
		if ta.prio != tb.prio {
			return ta.prio > tb.prio
		}
		if ta.ws != tb.ws {
			return ta.ws < tb.ws
		}
		return ta.id < tb.id
	})
	levels := []int{}
	for _, idx := range order {
		if len(levels) == 0 || levels[len(levels)-1] != tasks[idx].prio {
			levels = append(levels, tasks[idx].prio)
		}
	}
	levelOf := map[int]int{}
	for i, l := range levels {
		levelOf[l] = i
	}
	remaining := make([]int, len(levels))
	for _, t := range tasks {
		remaining[levelOf[t.prio]]++
	}

	closure := closureStarts(tasks, fixed)
	cands := make([][]int64, len(tasks))
	for i, t := range tasks {
		cands[i] = candidateStarts(t, closure)
	}

	best := &planScore{sat: make([]int, len(levels)), offset: math.MaxInt64, keepMiss: math.MaxInt64, startSum: math.MaxInt64}
	var bestStarts []int64
	curSat := make([]int, len(levels))
	curStarts := make([]int64, len(tasks))
	for i := range curStarts {
		curStarts[i] = -1
	}
	placed := append([]solveInterval(nil), fixed...)

	var dfs func(pos int, offset int64, keepMiss int, startSum int64)
	dfs = func(pos int, offset int64, keepMiss int, startSum int64) {
		if bestStarts != nil {
			// 上界剪枝：剩余任务全部满足也无法超越当前最优时直接返回。
			rem := make([]int, len(levels))
			copy(rem, remaining)
			for k := 0; k < pos; k++ {
				rem[levelOf[tasks[order[k]].prio]]--
			}
			worse := false
			equalSat := true
			for l := range levels {
				potential := curSat[l] + rem[l]
				if potential < best.sat[l] {
					worse = true
					break
				}
				if potential > best.sat[l] {
					equalSat = false
					break
				}
			}
			if worse {
				return
			}
			if equalSat {
				if offset > best.offset {
					return
				}
				if offset == best.offset && keepMiss > best.keepMiss {
					return
				}
			}
		}
		if pos == len(order) {
			score := &planScore{
				sat:      append([]int(nil), curSat...),
				offset:   offset,
				keepMiss: keepMiss,
				startSum: startSum,
				starts:   append([]int64(nil), curStarts...),
			}
			if bestStarts == nil || betterScore(score, best) {
				best = score
				bestStarts = append([]int64(nil), curStarts...)
			}
			return
		}
		idx := order[pos]
		t := tasks[idx]
		for _, start := range cands[idx] {
			iv := solveInterval{start: start, end: start + t.dur, power: t.power, device: t.device, id: t.id}
			if !intervalFits(placed, iv, cap) {
				continue
			}
			km := keepMiss
			if !t.hasKeep || t.keep != start {
				km++
			}
			curSat[levelOf[t.prio]]++
			curStarts[idx] = start
			placed = append(placed, iv)
			dfs(pos+1, offset+start-t.ws, km, startSum+start)
			placed = placed[:len(placed)-1]
			curStarts[idx] = -1
			curSat[levelOf[t.prio]]--
		}
		// 跳过该任务（保持未安排）。
		dfs(pos+1, offset, keepMiss, startSum)
	}
	dfs(0, 0, 0, 0)

	result := solveResult{placement: map[string]int64{}}
	committed := append([]solveInterval(nil), fixed...)
	for i, t := range tasks {
		if bestStarts != nil && bestStarts[i] >= 0 {
			result.placement[t.id] = bestStarts[i]
			committed = append(committed, solveInterval{start: bestStarts[i], end: bestStarts[i] + t.dur, power: t.power, device: t.device, id: t.id})
		}
	}
	for _, t := range tasks {
		if _, ok := result.placement[t.id]; ok {
			continue
		}
		result.dropped = append(result.dropped, explainDrop(t, committed, cap))
	}
	sort.Slice(result.dropped, func(i, j int) bool { return result.dropped[i].ID < result.dropped[j].ID })
	return result
}

// explainDrop 为未能安排的预约找出真实的冲突预约与超限区间，绝不静默缩短任务。
func explainDrop(t solveTask, committed []solveInterval, cap float64) DroppedTask {
	conflictSet := map[string]bool{}
	intervals := []OverloadInterval{}
	closure := closureStarts([]solveTask{t}, committed)
	for _, start := range candidateStarts(t, closure) {
		end := start + t.dur
		pts := []int64{start, end}
		for _, p := range committed {
			if p.start < end && start < p.end {
				pts = append(pts, max64(p.start, start), min64(p.end, end))
			}
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
		pts = uniqueInts(pts)
		for i := 0; i+1 < len(pts); i++ {
			a, b := pts[i], pts[i+1]
			if b <= a {
				continue
			}
			load := t.power
			ids := []string{}
			deviceBlock := false
			for _, p := range committed {
				if p.start <= a && a < p.end {
					load += p.power
					ids = append(ids, p.id)
					if p.device == t.device {
						deviceBlock = true
					}
				}
			}
			if load > cap+1e-9 || deviceBlock {
				for _, id := range ids {
					conflictSet[id] = true
				}
				required := load
				if deviceBlock && required <= cap {
					required = cap + t.power
				}
				intervals = append(intervals, OverloadInterval{Start: a, End: b, RequiredKW: required, LimitKW: cap, SourceIDs: ids})
			}
		}
	}
	conflicts := make([]string, 0, len(conflictSet))
	for id := range conflictSet {
		conflicts = append(conflicts, id)
	}
	sort.Strings(conflicts)
	return DroppedTask{ID: t.id, Conflicts: conflicts, Overloads: mergeOverloads(intervals)}
}

func mergeOverloads(in []OverloadInterval) []OverloadInterval {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Start != in[j].Start {
			return in[i].Start < in[j].Start
		}
		return in[i].End < in[j].End
	})
	out := []OverloadInterval{in[0]}
	for _, iv := range in[1:] {
		last := &out[len(out)-1]
		if iv.Start <= last.End {
			if iv.End > last.End {
				last.End = iv.End
			}
			if iv.RequiredKW > last.RequiredKW {
				last.RequiredKW = iv.RequiredKW
			}
			for _, id := range iv.SourceIDs {
				last.SourceIDs = appendUnique(last.SourceIDs, id)
			}
			sort.Strings(last.SourceIDs)
			continue
		}
		out = append(out, iv)
	}
	return out
}

func uniqueInts(in []int64) []int64 {
	out := in[:0]
	var prev int64
	for i, v := range in {
		if i > 0 && v == prev {
			continue
		}
		out = append(out, v)
		prev = v
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
