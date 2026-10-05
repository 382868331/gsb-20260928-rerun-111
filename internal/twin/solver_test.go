package twin

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// bruteForceBest 独立穷举候选开始时间闭包上的所有组合（含不安排），
// 只做可行性剪枝、不做目标剪枝，返回与求解器相同口径的最优（逐级满足数，总偏移）。
func bruteForceBest(tasks []solveTask, fixed []solveInterval, cap float64) ([]int, int64) {
	levels := []int{}
	for _, t := range tasks {
		found := false
		for _, l := range levels {
			if l == t.prio {
				found = true
			}
		}
		if !found {
			levels = append(levels, t.prio)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(levels)))
	levelOf := map[int]int{}
	for i, l := range levels {
		levelOf[l] = i
	}
	closure := closureStarts(tasks, fixed)
	cands := make([][]int64, len(tasks))
	for i, t := range tasks {
		cands[i] = append(candidateStarts(t, closure), -1) // -1 表示不安排
	}
	var bestSat []int
	bestOffset := int64(-1)
	sat := make([]int, len(levels))
	placed := append([]solveInterval(nil), fixed...)
	var rec func(i int, offset int64)
	rec = func(i int, offset int64) {
		if i == len(tasks) {
			if bestSat == nil {
				bestSat, bestOffset = append([]int(nil), sat...), offset
				return
			}
			for l := range sat {
				if sat[l] != bestSat[l] {
					if sat[l] > bestSat[l] {
						bestSat, bestOffset = append([]int(nil), sat...), offset
					}
					return
				}
			}
			if offset < bestOffset {
				bestSat, bestOffset = append([]int(nil), sat...), offset
			}
			return
		}
		t := tasks[i]
		for _, c := range cands[i] {
			if c < 0 {
				rec(i+1, offset)
				continue
			}
			iv := solveInterval{start: c, end: c + t.dur, power: t.power, device: t.device, id: t.id}
			if !intervalFits(placed, iv, cap) {
				continue
			}
			placed = append(placed, iv)
			sat[levelOf[t.prio]]++
			rec(i+1, offset+c-t.ws)
			sat[levelOf[t.prio]]--
			placed = placed[:len(placed)-1]
		}
	}
	rec(0, 0)
	return bestSat, bestOffset
}

func solverScore(res solveResult, tasks []solveTask) ([]int, int64) {
	levels := []int{}
	for _, t := range tasks {
		found := false
		for _, l := range levels {
			if l == t.prio {
				found = true
			}
		}
		if !found {
			levels = append(levels, t.prio)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(levels)))
	sat := make([]int, len(levels))
	offset := int64(0)
	for _, t := range tasks {
		start, ok := res.placement[t.id]
		if !ok {
			continue
		}
		for i, l := range levels {
			if l == t.prio {
				sat[i]++
			}
		}
		offset += start - t.ws
	}
	return sat, offset
}

// TestSolverMatchesBruteForce 用多设备交叉窗口的随机小规模案例，
// 核对求解器结果与穷举最优一致（满足数逐级相同、总偏移相同）。
func TestSolverMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20261006))
	for trial := 0; trial < 300; trial++ {
		n := 2 + rng.Intn(4) // 2..5 个任务
		cap := float64(4 + rng.Intn(8))
		tasks := make([]solveTask, n)
		for i := 0; i < n; i++ {
			ws := int64(rng.Intn(12)) * 300
			we := ws + int64(rng.Intn(10))*300
			dur := int64(1+rng.Intn(6)) * 300
			tasks[i] = solveTask{
				id:     fmt.Sprintf("T%d", i),
				device: fmt.Sprintf("dev-%d", rng.Intn(3)),
				ws:     ws,
				we:     we,
				dur:    dur,
				power:  float64(1 + rng.Intn(6)),
				prio:   rng.Intn(4),
			}
		}
		fixed := []solveInterval{}
		if rng.Intn(2) == 0 {
			fs := int64(rng.Intn(6)) * 300
			fixed = append(fixed, solveInterval{
				start:  fs,
				end:    fs + int64(1+rng.Intn(4))*300,
				power:  float64(1 + rng.Intn(4)),
				device: "dev-fixed",
				id:     "FIXED",
			})
		}
		res := solvePowerPlan(tasks, fixed, cap)
		gotSat, gotOffset := solverScore(res, tasks)
		wantSat, wantOffset := bruteForceBest(tasks, fixed, cap)
		if len(gotSat) != len(wantSat) {
			t.Fatalf("trial %d: sat 维度不一致", trial)
		}
		for l := range gotSat {
			if gotSat[l] != wantSat[l] {
				t.Fatalf("trial %d: 优先级层级 %d 满足数 %d, 穷举最优 %d (cap=%v tasks=%+v fixed=%+v)",
					trial, l, gotSat[l], wantSat[l], cap, tasks, fixed)
			}
		}
		if gotOffset != wantOffset {
			t.Fatalf("trial %d: 总启动偏移 %d, 穷举最优 %d (cap=%v tasks=%+v fixed=%+v)",
				trial, gotOffset, wantOffset, cap, tasks, fixed)
		}
		// 验证计划本身满足容量约束且时长未被截断。
		placed := append([]solveInterval(nil), fixed...)
		for _, task := range tasks {
			start, ok := res.placement[task.id]
			if !ok {
				continue
			}
			iv := solveInterval{start: start, end: start + task.dur, power: task.power, device: task.device, id: task.id}
			if !intervalFits(placed, iv, cap) {
				t.Fatalf("trial %d: 任务 %s 的计划违反容量约束", trial, task.id)
			}
			placed = append(placed, iv)
		}
	}
}

// TestSolverBeatsGreedyOrder 构造贪心（按优先级从早到晚放置）会错失可行安排、
// 但全局求解能找到的场景：高优先级任务占满窗口起点会挤掉低优先级任务，
// 只有让高优先级任务偏移才能两者都满足。
func TestSolverBeatsGreedyOrder(t *testing.T) {
	cap := 10.0
	high := solveTask{id: "HIGH", device: "dev-1", ws: 0, we: 50 * 60, dur: 50 * 60, power: 7, prio: 5}
	low := solveTask{id: "LOW", device: "dev-2", ws: 0, we: 10 * 60, dur: 10 * 60, power: 5, prio: 1}
	tasks := []solveTask{high, low}

	// 模拟贪心：按优先级降序，各自取最早可行开始时间。
	greedyPlaced := []solveInterval{}
	greedySat := 0
	closure := closureStarts(tasks, nil)
	for _, task := range []solveTask{high, low} {
		placed := false
		for _, start := range candidateStarts(task, closure) {
			iv := solveInterval{start: start, end: start + task.dur, power: task.power, device: task.device, id: task.id}
			if intervalFits(greedyPlaced, iv, cap) {
				greedyPlaced = append(greedyPlaced, iv)
				greedySat++
				placed = true
				break
			}
		}
		_ = placed
	}
	if greedySat != 1 {
		t.Fatalf("贪心在该场景应只满足 1 个任务，实际 %d", greedySat)
	}

	res := solvePowerPlan(tasks, nil, cap)
	if len(res.placement) != 2 {
		t.Fatalf("求解器应找到两个任务都可行的安排，实际 %v", res.placement)
	}
	if res.placement["LOW"] != 0 {
		t.Fatalf("低优先级任务应在窗口起点开始，实际 %d", res.placement["LOW"])
	}
	if res.placement["HIGH"] != 10*60 {
		t.Fatalf("高优先级任务应偏移到低任务结束后开始，实际 %d", res.placement["HIGH"])
	}
}

// TestSolverInfeasibleReportsConflicts 不可行时必须给出真实冲突预约与超限区间。
func TestSolverInfeasibleReportsConflicts(t *testing.T) {
	cap := 10.0
	mk := func(id string, prio int) solveTask {
		return solveTask{id: id, device: "dev-" + id, ws: 0, we: 0, dur: 30 * 60, power: 6, prio: prio}
	}
	tasks := []solveTask{mk("A", 3), mk("B", 2), mk("C", 1)}
	res := solvePowerPlan(tasks, nil, cap)
	if len(res.placement) != 1 {
		t.Fatalf("容量 10、每个任务 6kW 同时段只能安排 1 个，实际 %v", res.placement)
	}
	if _, ok := res.placement["A"]; !ok {
		t.Fatalf("最高优先级任务 A 应被安排，实际 %v", res.placement)
	}
	if len(res.dropped) != 2 {
		t.Fatalf("应有 2 个任务无法安排，实际 %v", res.dropped)
	}
	for _, d := range res.dropped {
		if len(d.Conflicts) == 0 || d.Conflicts[0] != "A" {
			t.Fatalf("任务 %s 的冲突预约应包含 A，实际 %v", d.ID, d.Conflicts)
		}
		if len(d.Overloads) == 0 {
			t.Fatalf("任务 %s 应给出超限区间", d.ID)
		}
		ov := d.Overloads[0]
		if ov.RequiredKW <= ov.LimitKW {
			t.Fatalf("超限区间 required(%v) 应大于 limit(%v)", ov.RequiredKW, ov.LimitKW)
		}
		if ov.RequiredKW != 12 {
			t.Fatalf("两个 6kW 叠加应为 12kW，实际 %v", ov.RequiredKW)
		}
	}
}

// TestSolverChainedStarts 一个任务的开始依赖另一个任务的结束、再依赖固定占用结束：
// 候选开始时间闭包必须覆盖这种链式位移。
func TestSolverChainedStarts(t *testing.T) {
	cap := 6.0
	fixed := []solveInterval{{start: 0, end: 3600, power: 6, device: "dev-0", id: "RUN"}}
	b := solveTask{id: "B", device: "dev-1", ws: 0, we: 7200, dur: 1800, power: 6, prio: 1, keep: 3600, hasKeep: true}
	e := solveTask{id: "E", device: "dev-2", ws: 300, we: 7200, dur: 1800, power: 6, prio: 100}
	res := solvePowerPlan([]solveTask{b, e}, fixed, cap)
	if res.placement["B"] != 3600 {
		t.Fatalf("B 应保持 3600，实际 %v", res.placement)
	}
	if res.placement["E"] != 5400 {
		t.Fatalf("E 应紧跟 B 结束于 5400 开始，实际 %v", res.placement)
	}
	if len(res.dropped) != 0 {
		t.Fatalf("两个任务都应可安排，实际丢弃 %v", res.dropped)
	}
}
