package main

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// setMemoryLimit sets the Go memory limit to 90% of the container's memory
// limit (cgroup v2), leaving room for memory the Go runtime does not manage,
// unless GOMEMLIMIT is set (GW-27). GOMAXPROCS needs nothing: since Go 1.25
// it follows the container's CPU limit by itself.
func setMemoryLimit(log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return // not in a cgroup v2 container
	}
	if limit, ok := parseMemoryMax(data); ok {
		debug.SetMemoryLimit(limit / 10 * 9)
		log.Info("memory limit set from the container limit", "container_bytes", limit, "gomemlimit_bytes", limit/10*9)
	}
}

// parseMemoryMax parses cgroup v2 memory.max: a byte count, or "max" for none.
func parseMemoryMax(data []byte) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n, err == nil && n > 0
}
