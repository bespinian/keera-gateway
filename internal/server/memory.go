package server

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// cgroupMemoryMax is where cgroup v2 shows a container its memory limit.
const cgroupMemoryMax = "/sys/fs/cgroup/memory.max"

// setMemoryLimit keeps the gateway under its container's memory limit, so a
// burst of large requests makes the garbage collector work harder rather than
// get the process killed. A tenth is left for memory Go does not count.
// GOMEMLIMIT, if set, wins.
func setMemoryLimit(log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	limit, ok := memoryLimit(cgroupMemoryMax)
	if !ok {
		return
	}
	debug.SetMemoryLimit(limit)
	log.Info("memory limit taken from the container", "bytes", limit)
}

// memoryLimit reads a cgroup's memory.max and returns nine tenths of it, or
// false when there is no limit or no such file.
func memoryLimit(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	// "max" means no limit.
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n / 10 * 9, true
}
