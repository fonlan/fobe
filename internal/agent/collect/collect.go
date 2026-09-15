// Package collect reads hardware metrics straight from /proc, /sys and
// statfs (design.md §8.1) — no external commands, so busybox OpenWrt works.
// The linux files carry the real implementation; a stub keeps non-linux
// builds compiling for editor/dev use only.
package collect

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
)

// Snapshot is one observation of the collector.
type Snapshot struct {
	Metrics   protocol.Metrics
	TrafficRx uint64 // cumulative counters of the selected iface
	TrafficTx uint64
	HasIface  bool
}

// Collector keeps the previous /proc samples needed for deltas.
type Collector struct {
	mu      sync.Mutex
	prevCPU cpuTimes
	hasCPU  bool
	prevNet map[string]netCounters
	lastNet time.Time
}

type cpuTimes struct {
	idle, total uint64
}

func New() *Collector {
	return &Collector{prevNet: map[string]netCounters{}}
}

// Read gathers a full snapshot. iface selects the traffic interface
// ("" = first non-loopback with counters).
func (c *Collector) Read(iface string) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	m := c.readMetrics()
	snap := Snapshot{Metrics: m}
	rx, tx, ok := c.readNet(iface, &snap.Metrics)
	snap.TrafficRx, snap.TrafficTx, snap.HasIface = rx, tx, ok
	return snap
}

func (c *Collector) readMetrics() protocol.Metrics {
	m := protocol.Metrics{}
	m.CPU = c.readCPU()
	m.Load1 = readLoad()
	m.MemUsed, m.MemTotal, m.SwapUsed, m.SwapTotal = readMem()
	m.Disks = readDisks()
	m.Uptime = readUptime()
	return m
}

// readCPU computes usage from the delta of /proc/stat since the previous call.
func (c *Collector) readCPU() float64 {
	idle, total, ok := parseCPU()
	if !ok {
		return 0
	}
	if !c.hasCPU {
		c.prevCPU, c.hasCPU = cpuTimes{idle, total}, true
		return 0
	}
	dIdle := float64(idle - c.prevCPU.idle)
	dTotal := float64(total - c.prevCPU.total)
	c.prevCPU = cpuTimes{idle, total}
	if dTotal <= 0 {
		return 0
	}
	usage := (dTotal - dIdle) / dTotal * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return usage
}

func parseCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var vals []uint64
		for _, fs := range fields {
			v, err := strconv.ParseUint(fs, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals = append(vals, v)
		}
		// user nice system idle iowait irq softirq steal ...
		var sum uint64
		for _, v := range vals {
			sum += v
		}
		idle = vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait counts as idle
		}
		return idle, sum, true
	}
	return 0, 0, false
}

func readLoad() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// readMem prefers MemAvailable and falls back to MemFree+Buffers+Cached
// (OpenWrt kernels don't always expose MemAvailable, §8.1).
func readMem() (used, total, swapUsed, swapTotal uint64) {
	vals := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		vals[strings.TrimSuffix(parts[0], ":")] = v * 1024 // kB → bytes
	}
	total = vals["MemTotal"]
	if avail, ok := vals["MemAvailable"]; ok {
		used = total - avail
	} else {
		used = total - (vals["MemFree"] + vals["Buffers"] + vals["Cached"])
	}
	swapTotal = vals["SwapTotal"]
	swapUsed = swapTotal - vals["SwapFree"]
	return
}

func readUptime() uint64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return uint64(v)
}

// BootID identifies the current boot for restart detection (§8.1).
func BootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// CPUCores reads /proc/cpuinfo.
func CPUCores() int {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 1
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// Kernel returns `uname -r` equivalent from /proc/sys/kernel/osrelease.
func Kernel() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
