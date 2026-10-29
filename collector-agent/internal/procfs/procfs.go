// Package procfs parses Linux /proc pseudo-files into typed structures.
package procfs

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SectorSize is the unit used by /proc/diskstats regardless of device block size.
const SectorSize = 512

// Pool for reusable read buffers to eliminate heap allocation during scraping.
var readBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32768)
		return &b
	},
}

// Pool for CPUStat slices.
var cpuSlicePool = sync.Pool{
	New: func() any {
		s := make([]CPUStat, 0, 64)
		return &s
	},
}

// PutCPUStats returns a CPUStat slice to the internal pool for reuse.
func PutCPUStats(s []CPUStat) {
	if cap(s) > 0 {
		s = s[:0]
		cpuSlicePool.Put(&s)
	}
}

// Static table of interned CPU names for up to 256 cores to avoid string allocations.
var cpuNames = func() [257]string {
	var names [257]string
	names[0] = "cpu"
	for i := 0; i < 256; i++ {
		names[i+1] = fmt.Sprintf("cpu%d", i)
	}
	return names
}()

func internCPUName(name []byte) string {
	if len(name) == 3 && name[0] == 'c' && name[1] == 'p' && name[2] == 'u' {
		return cpuNames[0] // "cpu"
	}
	if len(name) > 3 && name[0] == 'c' && name[1] == 'p' && name[2] == 'u' {
		idx, err := parseUintBytes(name[3:])
		if err == nil && idx < 256 {
			return cpuNames[idx+1]
		}
	}
	return string(name)
}

func readAllPooled(r io.Reader) (*[]byte, int, error) {
	bufPtr := readBufPool.Get().(*[]byte)
	buf := *bufPtr
	n := 0
	for {
		if n == len(buf) {
			newBuf := make([]byte, len(buf)*2)
			copy(newBuf, buf)
			buf = newBuf
			*bufPtr = buf
		}
		readBytes, err := r.Read(buf[n:])
		n += readBytes
		if err != nil {
			if err == io.EOF {
				return bufPtr, n, nil
			}
			readBufPool.Put(bufPtr)
			return nil, 0, err
		}
	}
}

func nextLine(data []byte) (line, rest []byte) {
	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		return trimCR(data), nil
	}
	return trimCR(data[:idx]), data[idx+1:]
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

func nextField(data []byte) (field, rest []byte) {
	i := 0
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	if i == len(data) {
		return nil, nil
	}
	start := i
	for i < len(data) && data[i] != ' ' && data[i] != '\t' {
		i++
	}
	return data[start:i], data[i:]
}

func parseUintBytes(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, strconv.ErrSyntax
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
		d := uint64(c - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, strconv.ErrRange
		}
		n = n*10 + d
	}
	return n, nil
}

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

// ParseStatInto parses /proc/stat into dst, reusing capacity to avoid allocations.
func ParseStatInto(r io.Reader, dst []CPUStat) ([]CPUStat, error) {
	bufPtr, n, err := readAllPooled(r)
	if err != nil {
		return dst, err
	}
	defer readBufPool.Put(bufPtr)

	out := dst[:0]
	rest := (*bufPtr)[:n]

	for len(rest) > 0 {
		var line []byte
		line, rest = nextLine(rest)
		if len(line) == 0 {
			continue
		}
		f0, lineRest := nextField(line)
		if len(f0) < 3 || f0[0] != 'c' || f0[1] != 'p' || f0[2] != 'u' {
			continue
		}

		var vals [8]uint64
		fieldCount := 0
		fieldCur := lineRest
		hasErr := false
		var parseErr error

		for fieldCount < 8 {
			var f []byte
			f, fieldCur = nextField(fieldCur)
			if len(f) == 0 {
				break
			}
			v, err := parseUintBytes(f)
			if err != nil {
				hasErr = true
				parseErr = fmt.Errorf("procfs: bad cpu field %q: %w", string(f), err)
				break
			}
			vals[fieldCount] = v
			fieldCount++
		}

		if hasErr {
			return nil, parseErr
		}
		if fieldCount < 8 {
			return nil, fmt.Errorf("procfs: short cpu line %q", string(line))
		}

		out = append(out, CPUStat{
			Name:    internCPUName(f0),
			User:    vals[0],
			Nice:    vals[1],
			System:  vals[2],
			Idle:    vals[3],
			IOWait:  vals[4],
			IRQ:     vals[5],
			SoftIRQ: vals[6],
			Steal:   vals[7],
		})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("procfs: no cpu lines found")
	}
	return out, nil
}

// ParseStat parses /proc/stat and returns the aggregate line first, then per-core lines.
func ParseStat(r io.Reader) ([]CPUStat, error) {
	ptr := cpuSlicePool.Get().(*[]CPUStat)
	res, err := ParseStatInto(r, (*ptr)[:0])
	if err != nil {
		cpuSlicePool.Put(ptr)
		return nil, err
	}
	return res, nil
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
	bufPtr, n, err := readAllPooled(r)
	if err != nil {
		return MemInfo{}, err
	}
	defer readBufPool.Put(bufPtr)

	var m MemInfo
	seenTotal := false
	seenAvail := false

	rest := (*bufPtr)[:n]
	for len(rest) > 0 {
		var line []byte
		line, rest = nextLine(rest)
		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		valField, unitField := nextField(line[colon+1:])
		if len(valField) == 0 {
			continue
		}
		v, err := parseUintBytes(valField)
		if err != nil {
			return m, fmt.Errorf("procfs: bad meminfo value for %s: %w", string(key), err)
		}
		unit, _ := nextField(unitField)
		if len(unit) == 2 && (unit[0] == 'k' || unit[0] == 'K') && (unit[1] == 'B' || unit[1] == 'b') {
			v *= 1024
		}

		switch string(key) {
		case "MemTotal":
			m.Total = v
			seenTotal = true
		case "MemFree":
			m.Free = v
		case "MemAvailable":
			m.Available = v
			seenAvail = true
		case "Buffers":
			m.Buffers = v
		case "Cached":
			m.Cached = v
		case "SwapTotal":
			m.SwapTotal = v
		case "SwapFree":
			m.SwapFree = v
		}
	}

	if !seenTotal {
		return m, fmt.Errorf("procfs: MemTotal missing")
	}
	if !seenAvail { // very old kernels
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

// ParseDiskStatsInto parses /proc/diskstats into dst, skipping loop and ram devices.
func ParseDiskStatsInto(r io.Reader, dst []DiskStat) ([]DiskStat, error) {
	bufPtr, n, err := readAllPooled(r)
	if err != nil {
		return dst, err
	}
	defer readBufPool.Put(bufPtr)

	out := dst[:0]
	rest := (*bufPtr)[:n]
	for len(rest) > 0 {
		var line []byte
		line, rest = nextLine(rest)
		if len(line) == 0 {
			continue
		}
		cur := line
		var f [14][]byte
		count := 0
		for count < 14 {
			var field []byte
			field, cur = nextField(cur)
			if len(field) == 0 {
				break
			}
			f[count] = field
			count++
		}
		if count == 0 {
			continue
		}
		if count < 14 {
			return nil, fmt.Errorf("procfs: short diskstats line %q", string(line))
		}
		name := string(f[2])
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}

		p := func(idx int) (uint64, error) {
			v, err := parseUintBytes(f[idx])
			if err != nil {
				return 0, fmt.Errorf("procfs: bad diskstats field %q: %w", string(f[idx]), err)
			}
			return v, nil
		}

		var d DiskStat
		d.Device = name
		var err error
		if d.ReadsDone, err = p(3); err != nil {
			return nil, err
		}
		if d.SectorsRead, err = p(5); err != nil {
			return nil, err
		}
		if d.ReadMs, err = p(6); err != nil {
			return nil, err
		}
		if d.WritesDone, err = p(7); err != nil {
			return nil, err
		}
		if d.SectorsWrite, err = p(9); err != nil {
			return nil, err
		}
		if d.WriteMs, err = p(10); err != nil {
			return nil, err
		}
		if d.IOInProgress, err = p(11); err != nil {
			return nil, err
		}
		if d.IOMs, err = p(12); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// ParseDiskStats parses /proc/diskstats, skipping loop and ram devices.
func ParseDiskStats(r io.Reader) ([]DiskStat, error) {
	return ParseDiskStatsInto(r, nil)
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

// ParseNetDevInto parses /proc/net/dev into dst.
func ParseNetDevInto(r io.Reader, dst []NetStat) ([]NetStat, error) {
	bufPtr, n, err := readAllPooled(r)
	if err != nil {
		return dst, err
	}
	defer readBufPool.Put(bufPtr)

	out := dst[:0]
	rest := (*bufPtr)[:n]
	for len(rest) > 0 {
		var line []byte
		line, rest = nextLine(rest)
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(string(line[:colon]))
		cur := line[colon+1:]

		var vals [16]uint64
		count := 0
		hasErr := false
		var parseErr error

		for count < 16 {
			var field []byte
			field, cur = nextField(cur)
			if len(field) == 0 {
				break
			}
			v, err := parseUintBytes(field)
			if err != nil {
				hasErr = true
				parseErr = fmt.Errorf("procfs: bad net/dev field %q: %w", string(field), err)
				break
			}
			vals[count] = v
			count++
		}
		if hasErr {
			return nil, parseErr
		}
		if count < 16 {
			return nil, fmt.Errorf("procfs: short net/dev line %q", string(line))
		}

		out = append(out, NetStat{
			Interface: name,
			RxBytes:   vals[0], RxPackets: vals[1], RxErrors: vals[2], RxDropped: vals[3],
			TxBytes: vals[8], TxPackets: vals[9], TxErrors: vals[10], TxDropped: vals[11],
		})
	}
	return out, nil
}

// ParseNetDev parses /proc/net/dev.
func ParseNetDev(r io.Reader) ([]NetStat, error) {
	return ParseNetDevInto(r, nil)
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

// ReadInto collects a Snapshot into an existing Snapshot structure, reusing slices.
func (f FS) ReadInto(s *Snapshot) error {
	parse := func(rel string, fn func(io.Reader) error) error {
		fh, err := f.open(rel)
		if err != nil {
			return err
		}
		defer fh.Close()
		return fn(fh)
	}
	if err := parse("stat", func(r io.Reader) (err error) { s.CPUs, err = ParseStatInto(r, s.CPUs); return }); err != nil {
		return err
	}
	if err := parse("meminfo", func(r io.Reader) (err error) { s.Mem, err = ParseMemInfo(r); return }); err != nil {
		return err
	}
	if err := parse("diskstats", func(r io.Reader) (err error) { s.Disks, err = ParseDiskStatsInto(r, s.Disks); return }); err != nil {
		return err
	}
	if err := parse("net/dev", func(r io.Reader) (err error) { s.Nets, err = ParseNetDevInto(r, s.Nets); return }); err != nil {
		return err
	}
	return nil
}

// Read collects a Snapshot from the proc filesystem.
func (f FS) Read() (Snapshot, error) {
	var s Snapshot
	err := f.ReadInto(&s)
	return s, err
}
