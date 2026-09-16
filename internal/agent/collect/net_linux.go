//go:build linux

package collect

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

// readNet returns the cumulative rx/tx counters of iface (per /proc/net/dev),
// and updates the rate fields from the delta since the previous read. Empty
// iface means the default-route interface, never an unstable map iteration.
func (c *Collector) readNet(iface string, m *protocol.Metrics) (name string, rx, tx uint64, ok bool) {
	counters := parseNetDev()
	if len(counters) == 0 {
		return "", 0, 0, false
	}
	name = iface
	if name == "" {
		name = defaultInterface(counters)
	}
	ct, found := counters[name]
	if !found {
		return "", 0, 0, false
	}

	if prev, seen := c.prevNet[name]; seen && c.lastNet.IsZero() == false {
		dt := time.Since(c.lastNet).Seconds()
		if dt > 0 {
			if dRx := float64(ct.rx) - float64(prev.rx); dRx > 0 {
				m.NetRxRate = dRx / dt
			}
			if dTx := float64(ct.tx) - float64(prev.tx); dTx > 0 {
				m.NetTxRate = dTx / dt
			}
		}
	}
	c.prevNet[name] = ct
	c.lastNet = time.Now()
	return name, ct.rx, ct.tx, true
}

type netCounters struct {
	rx, tx uint64
}

func parseNetDev() map[string]netCounters {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]netCounters{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 { // two header rows
			continue
		}
		parts := strings.SplitN(sc.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		out[name] = netCounters{rx: rx, tx: tx}
	}
	return out
}

// NetworkInterfaces is the stable inventory sent to the panel. It intentionally
// reads procfs rather than invoking ip(8), which is absent or inconsistent on
// OpenWrt installations (design §5.1).
func NetworkInterfaces() []protocol.NetworkInterface {
	counters := parseNetDev()
	if len(counters) == 0 {
		return []protocol.NetworkInterface{}
	}
	defaultName := defaultInterface(counters)
	names := make([]string, 0, len(counters))
	for name := range counters {
		if name != "lo" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]protocol.NetworkInterface, 0, len(names))
	for _, name := range names {
		out = append(out, protocol.NetworkInterface{Name: name, Default: name == defaultName})
	}
	return out
}

func defaultInterface(counters map[string]netCounters) string {
	if route, ok := defaultRouteInterface(); ok {
		if _, present := counters[route]; present && route != "lo" {
			return route
		}
	}
	names := make([]string, 0, len(counters))
	for name, ct := range counters {
		if name != "lo" && (ct.rx > 0 || ct.tx > 0) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		for name := range counters {
			if name != "lo" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func defaultRouteInterface() (string, bool) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 64)
		if err == nil && flags&0x1 != 0 {
			return fields[0], true
		}
	}
	return "", false
}

// readDisks statfs's every real mount point (§8.1: no external commands).
func readDisks() []protocol.Disk {
	out := make([]protocol.Disk, 0, 8)
	seen := map[string]bool{}
	for _, mp := range readMounts() {
		if seen[mp] {
			continue
		}
		seen[mp] = true
		// §8.1 主分区:挂载点必须是目录。单文件 bind mount(/etc/hosts、
		// /etc/resolv.conf、bind 进来的 agent 二进制)不是分区,底层还可能是
		// FUSE/virtiofs 这种容量乱报的文件系统——实测一个 bind 进来的文件报出
		// 254 TB,直接把服务端"取最大"的主分区选歪。
		if info, err := os.Stat(mp); err != nil || !info.IsDir() {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mp, &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := st.Blocks * bsize
		if total == 0 {
			continue
		}
		out = append(out, protocol.Disk{
			Path:       mp,
			Total:      total,
			Used:       (st.Blocks - st.Bfree) * bsize,
			InodeTotal: st.Files,
			InodeUsed:  st.Files - st.Ffree,
		})
	}
	return out
}

// readMounts lists the mount points worth statfs'ing. The parsing/pseudo-fs
// policy lives in collect.go: it is portable, so it can be unit-tested on a dev
// machine (no /proc/mounts there).
func readMounts() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseMounts(f)
}
