// Package procfs parses Linux /proc pseudo-files into typed structures.
package procfs

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SectorSize is the unit used by /proc/diskstats regardless of device block size.
const SectorSize = 512

// CPUStat holds cumulative jiffies for one CPU line of /proc/stat.
type CPUStat struct {
	Name    string // "cpu" for the aggregate, "cpu0".. for cores
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	IOWait  uint64
	IRQ     uint64
	SoftIRQ uint64
	Steal   uint64
}

// Total returns all accounted jiffies.
func (c CPUStat) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// Busy returns non-idle jiffies (iowait counts as idle).
func (c CPUStat) Busy() uint64 {
	return c.Total() - c.Idle - c.IOWait
}

// ParseStat parses /proc/stat and returns the aggregate line first, then per-core lines.
func ParseStat(r io.Reader) ([]CPUStat, error) {
	var out []CPUStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if len(f) < 8 {
			return nil, fmt.Errorf("procfs: short cpu line %q", sc.Text())
		}
		vals := make([]uint64, 8)
		for i := 0; i < 8; i++ {
			v, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("procfs: bad cpu field %q: %w", f[i+1], err)
			}
			vals[i] = v
		}
		out = append(out, CPUStat{
			Name: f[0], User: vals[0], Nice: vals[1], System: vals[2], Idle: vals[3],
			IOWait: vals[4], IRQ: vals[5], SoftIRQ: vals[6], Steal: vals[7],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("procfs: no cpu lines found")
	}
	return out, nil
}

// CPUUsage computes busy and iowait percentages between two samples of the same CPU.
func CPUUsage(prev, cur CPUStat) (busyPct, iowaitPct float64) {
	if cur.Total() <= prev.Total() {
		return 0, 0
	}
	dt := float64(cur.Total() - prev.Total())
	var db, dw float64
	if cur.Busy() >= prev.Busy() {
		db = float64(cur.Busy() - prev.Busy())
	}
	if cur.IOWait >= prev.IOWait {
		dw = float64(cur.IOWait - prev.IOWait)
	}
	return 100 * db / dt, 100 * dw / dt
}

// MemInfo holds selected /proc/meminfo values in bytes.
type MemInfo struct {
	Total     uint64
	Free      uint64
	Available uint64
	Buffers   uint64
	Cached    uint64
	SwapTotal uint64
	SwapFree  uint64
}

// ParseMemInfo parses /proc/meminfo (values are reported in kB).
func ParseMemInfo(r io.Reader) (MemInfo, error) {
	var m MemInfo
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return m, fmt.Errorf("procfs: bad meminfo value for %s: %w", key, err)
		}
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			v *= 1024
		}
		switch key {
		case "MemTotal":
			m.Total = v
		case "MemFree":
			m.Free = v
		case "MemAvailable":
			m.Available = v
		case "Buffers":
			m.Buffers = v
		case "Cached":
			m.Cached = v
		case "SwapTotal":
			m.SwapTotal = v
		case "SwapFree":
			m.SwapFree = v
		default:
			continue
		}
		seen[key] = true
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !seen["MemTotal"] {
		return m, fmt.Errorf("procfs: MemTotal missing")
	}
	if !seen["MemAvailable"] { // very old kernels
		m.Available = m.Free + m.Buffers + m.Cached
	}
	return m, nil
}

// UsedRatio returns the fraction of memory that is unavailable (0..1).
func (m MemInfo) UsedRatio() float64 {
	if m.Total == 0 {
		return 0
	}
	return 1 - float64(m.Available)/float64(m.Total)
}

// DiskStat holds cumulative counters for one block device.
type DiskStat struct {
	Device       string
	ReadsDone    uint64
	SectorsRead  uint64
	ReadMs       uint64
	WritesDone   uint64
	SectorsWrite uint64
	WriteMs      uint64
	IOInProgress uint64
	IOMs         uint64 // time spent doing I/O
}

// ReadBytes returns total bytes read.
func (d DiskStat) ReadBytes() uint64 { return d.SectorsRead * SectorSize }

// WriteBytes returns total bytes written.
func (d DiskStat) WriteBytes() uint64 { return d.SectorsWrite * SectorSize }

// ParseDiskStats parses /proc/diskstats, skipping loop and ram devices.
func ParseDiskStats(r io.Reader) ([]DiskStat, error) {
	var out []DiskStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 14 {
			return nil, fmt.Errorf("procfs: short diskstats line %q", sc.Text())
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		n := func(i int) (uint64, error) { return strconv.ParseUint(f[i], 10, 64) }
		var d DiskStat
		d.Device = name
		var err error
		for _, p := range []struct {
			idx int
			dst *uint64
		}{
			{3, &d.ReadsDone}, {5, &d.SectorsRead}, {6, &d.ReadMs},
			{7, &d.WritesDone}, {9, &d.SectorsWrite}, {10, &d.WriteMs},
			{11, &d.IOInProgress}, {12, &d.IOMs},
		} {
			if *p.dst, err = n(p.idx); err != nil {
				return nil, fmt.Errorf("procfs: bad diskstats field %q: %w", f[p.idx], err)
			}
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

// NetStat holds cumulative counters for one network interface.
type NetStat struct {
	Interface string
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDropped uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDropped uint64
}

// ParseNetDev parses /proc/net/dev.
func ParseNetDev(r io.Reader) ([]NetStat, error) {
	var out []NetStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok { // header lines have no colon-separated interface
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			return nil, fmt.Errorf("procfs: short net/dev line %q", sc.Text())
		}
		vals := make([]uint64, 16)
		for i := range vals {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("procfs: bad net/dev field %q: %w", f[i], err)
			}
			vals[i] = v
		}
		out = append(out, NetStat{
			Interface: strings.TrimSpace(name),
			RxBytes:   vals[0], RxPackets: vals[1], RxErrors: vals[2], RxDropped: vals[3],
			TxBytes: vals[8], TxPackets: vals[9], TxErrors: vals[10], TxDropped: vals[11],
		})
	}
	return out, sc.Err()
}

// Snapshot is a point-in-time reading of all supported sources.
type Snapshot struct {
	CPUs  []CPUStat
	Mem   MemInfo
	Disks []DiskStat
	Nets  []NetStat
}

// FS reads from a proc mount (e.g. "/proc" or "/host/proc" in containers).
type FS struct{ Root string }

func (f FS) open(rel string) (*os.File, error) { return os.Open(filepath.Join(f.Root, rel)) }

// Read collects a Snapshot from the proc filesystem.
func (f FS) Read() (Snapshot, error) {
	var s Snapshot
	parse := func(rel string, fn func(io.Reader) error) error {
		fh, err := f.open(rel)
		if err != nil {
			return err
		}
		defer fh.Close()
		return fn(fh)
	}
	if err := parse("stat", func(r io.Reader) (err error) { s.CPUs, err = ParseStat(r); return }); err != nil {
		return s, err
	}
	if err := parse("meminfo", func(r io.Reader) (err error) { s.Mem, err = ParseMemInfo(r); return }); err != nil {
		return s, err
	}
	if err := parse("diskstats", func(r io.Reader) (err error) { s.Disks, err = ParseDiskStats(r); return }); err != nil {
		return s, err
	}
	if err := parse("net/dev", func(r io.Reader) (err error) { s.Nets, err = ParseNetDev(r); return }); err != nil {
		return s, err
	}
	return s, nil
}
