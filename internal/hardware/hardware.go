// Package hardware describes the machine a measurement ran on, for loadgen
// and the accuracy harness (ADR-0014, ADR-0021).
package hardware

import (
	"os"
	"runtime"
	"strings"
)

// Info is the machine a result was measured on. Results are compared only
// between runs with the same CPU model (docs/design.md, Benchmark method;
// ADR-0003).
type Info struct {
	CPUModel   string `json:"cpu_model"`
	GOARCH     string `json:"goarch"`
	NumCPU     int    `json:"num_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`
}

// Read returns this machine's Info.
func Read() Info {
	info, _ := os.ReadFile("/proc/cpuinfo") // absent off Linux: no CPU model
	return Info{
		CPUModel:   cpuModel(info),
		GOARCH:     runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
}

// cpuModel is the first "model name" in /proc/cpuinfo. arm64 kernels list no
// model name, so there it is the core's implementer and part codes, such as
// "implementer 0x41 part 0xd0c" (Arm Neoverse N1).
func cpuModel(cpuinfo []byte) string {
	var implementer, part string
	for line := range strings.Lines(string(cpuinfo)) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "model name" && v != "":
			return v
		case k == "CPU implementer" && implementer == "":
			implementer = v
		case k == "CPU part" && part == "":
			part = v
		}
	}
	if implementer == "" || part == "" {
		return ""
	}
	return "implementer " + implementer + " part " + part
}
