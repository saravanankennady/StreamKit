package confine

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"syscall"
	"time"
)

const reapInterval = 5 * time.Second

// ReapOrphans clears up after processes that have lost their parent. Protect makes this process the one they come back to,
// and in a container it is PID 1 anyway, and nothing else would wait for them. It runs instead of an init process, which
// would hold this worker's environment in a process that is not protected.
//
// os/exec waits for the children it started, so only one still dead on the next pass is an orphan.
func ReapOrphans(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	seen := map[int]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seen = reapPass(seen)
		}
	}
}

func reapPass(seen map[int]bool) map[int]bool {
	next := map[int]bool{}
	for _, pid := range deadChildren(os.Getpid()) {
		if !seen[pid] {
			next[pid] = true
			continue
		}
		_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
	}
	return next
}

func deadChildren(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// A name is in parentheses and can hold anything, so fields are counted from the last closing one.
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			continue
		}
		fields := bytes.Fields(data[end+1:])
		if len(fields) < 2 || string(fields[0]) != "Z" {
			continue
		}
		if ppid, err := strconv.Atoi(string(fields[1])); err == nil && ppid == parent {
			out = append(out, pid)
		}
	}
	return out
}
