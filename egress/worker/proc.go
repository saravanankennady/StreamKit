package worker

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// clockTicks is the kernel's unit for CPU time in /proc. It is 100 on every Linux this runs on.
const clockTicks = 100

// procInfo is the part of /proc/<pid>/stat that matters here.
type procInfo struct {
	ppid  int
	ticks uint64
}

func readProc(pid int) (procInfo, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	return parseProcStat(data)
}

// parseProcStat reads one stat line. A process's name is in parentheses and can contain anything, including spaces and
// parentheses, so fields are counted from the last closing parenthesis.
func parseProcStat(data []byte) (procInfo, bool) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return procInfo{}, false
	}
	fields := bytes.Fields(data[end+1:])
	// After the name: state(0) ppid(1) ... utime(11) stime(12).
	if len(fields) < 13 {
		return procInfo{}, false
	}
	ppid, err1 := strconv.Atoi(string(fields[1]))
	utime, err2 := strconv.ParseUint(string(fields[11]), 10, 64)
	stime, err3 := strconv.ParseUint(string(fields[12]), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return procInfo{}, false
	}
	return procInfo{ppid: ppid, ticks: utime + stime}, true
}

func allProcs() map[int]procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make(map[int]procInfo, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if info, ok := readProc(pid); ok {
			out[pid] = info
		}
	}
	return out
}

// descendants returns a set of roots and everything below them.
func descendants(procs map[int]procInfo, roots []int) map[int]bool {
	children := make(map[int][]int, len(procs))
	for pid, info := range procs {
		children[info.ppid] = append(children[info.ppid], pid)
	}
	out := map[int]bool{}
	stack := append([]int{}, roots...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[pid] {
			continue
		}
		if _, alive := procs[pid]; !alive {
			continue
		}
		out[pid] = true
		stack = append(stack, children[pid]...)
	}
	return out
}

// cpuMeter adds up the CPU time of a job's processes, including Chrome's zygote and renderers. A process that exits
// between samples keeps the time last seen for it, so a short-lived helper is undercounted by at most one interval.
type cpuMeter struct {
	mu      sync.Mutex
	roots   map[int]bool
	seen    map[int]uint64
	started time.Time
}

func newCPUMeter() *cpuMeter {
	return &cpuMeter{roots: map[int]bool{}, seen: map[int]uint64{}, started: time.Now()}
}

func (m *cpuMeter) add(pid int) {
	m.mu.Lock()
	m.roots[pid] = true
	m.mu.Unlock()
}

// sample reads the current CPU time of everything in the job.
func (m *cpuMeter) sample() {
	m.mu.Lock()
	defer m.mu.Unlock()
	roots := make([]int, 0, len(m.roots))
	for pid := range m.roots {
		roots = append(roots, pid)
	}
	procs := allProcs()
	for pid := range descendants(procs, roots) {
		m.seen[pid] = procs[pid].ticks
	}
}

// seconds returns CPU time so far, and wall-clock time so far.
func (m *cpuMeter) seconds() (cpu, wall float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ticks uint64
	for _, t := range m.seen {
		ticks += t
	}
	return float64(ticks) / clockTicks, time.Since(m.started).Seconds()
}

// killTree kills a process, and everything it started. Its group catches what the tree no longer shows, such as the helpers
// of a browser process that has already died. The tree catches what has moved to a group of its own.
func killTree(pid int) {
	tree := descendants(allProcs(), []int{pid})
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for child := range tree {
		_ = syscall.Kill(child, syscall.SIGKILL)
	}
}

// childAttrs starts a process in its own group, and kills it if this worker dies, so a killed worker does not leave
// Chrome and ffmpeg running with nobody watching them.
func childAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
