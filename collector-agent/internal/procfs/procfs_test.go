package procfs

import (
	"math"
	"strings"
	"testing"
)

const statFixture = `cpu  100 10 50 800 20 5 5 10 0 0
cpu0 50 5 25 400 10 2 3 5 0 0
cpu1 50 5 25 400 10 3 2 5 0 0
intr 12345
ctxt 999
`

func TestParseStat(t *testing.T) {
	cpus, err := ParseStat(strings.NewReader(statFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(cpus) != 3 {
		t.Fatalf("want 3 cpu lines, got %d", len(cpus))
	}
	if cpus[0].Name != "cpu" || cpus[0].User != 100 || cpus[0].Steal != 10 {
		t.Errorf("unexpected aggregate: %+v", cpus[0])
	}
	if cpus[0].Total() != 1000 {
		t.Errorf("total = %d, want 1000", cpus[0].Total())
	}
	if cpus[0].Busy() != 180 {
		t.Errorf("busy = %d, want 180", cpus[0].Busy())
	}
}

func TestParseStatErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":  "",
		"short":  "cpu 1 2 3\n",
		"nonnum": "cpu 1 2 3 4 5 6 7 x\n",
		"nocpu":  "intr 1\n",
	} {
		if _, err := ParseStat(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCPUUsage(t *testing.T) {
	prev := CPUStat{User: 100, Idle: 800, IOWait: 100}
	cur := CPUStat{User: 160, Idle: 840, IOWait: 200} // dt=200 busy=60 iowait=100
	busy, iow := CPUUsage(prev, cur)
	if math.Abs(busy-30) > 1e-9 || math.Abs(iow-50) > 1e-9 {
		t.Errorf("busy=%v iowait=%v", busy, iow)
	}
	if b, w := CPUUsage(cur, prev); b != 0 || w != 0 {
		t.Errorf("counter regression should yield zero, got %v %v", b, w)
	}
}

const memFixture = `MemTotal:       16000000 kB
MemFree:         2000000 kB
MemAvailable:    8000000 kB
Buffers:          500000 kB
Cached:          4000000 kB
SwapTotal:       2000000 kB
SwapFree:        1000000 kB
Dirty:                 8 kB
`

func TestParseMemInfo(t *testing.T) {
	m, err := ParseMemInfo(strings.NewReader(memFixture))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16000000*1024 || m.Available != 8000000*1024 ||
		m.Buffers != 500000*1024 || m.Cached != 4000000*1024 {
		t.Errorf("unexpected: %+v", m)
	}
	if math.Abs(m.UsedRatio()-0.5) > 1e-9 {
		t.Errorf("used ratio = %v", m.UsedRatio())
	}
}

func TestParseMemInfoFallbackAndErrors(t *testing.T) {
	m, err := ParseMemInfo(strings.NewReader("MemTotal: 100 kB\nMemFree: 10 kB\nBuffers: 5 kB\nCached: 5 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Available != 20*1024 {
		t.Errorf("fallback available = %d", m.Available)
	}
	if _, err := ParseMemInfo(strings.NewReader("MemFree: 10 kB\n")); err == nil {
		t.Error("expected error without MemTotal")
	}
	if _, err := ParseMemInfo(strings.NewReader("MemTotal: abc kB\n")); err == nil {
		t.Error("expected error on bad value")
	}
}

const diskFixture = `   7       0 loop0 10 0 20 5 0 0 0 0 0 5 5 0 0 0 0 0 0
   8       0 sda 1000 10 20000 300 500 5 8000 200 2 1500 500 0 0 0 0 0 0
   8       1 sda1 900 10 19000 290 400 5 7000 190 0 1400 480 0 0 0 0 0 0
   1       0 ram0 0 0 0 0 0 0 0 0 0 0 0
`

func TestParseDiskStats(t *testing.T) {
	d, err := ParseDiskStats(strings.NewReader(diskFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 {
		t.Fatalf("loop/ram should be skipped, got %d devices", len(d))
	}
	sda := d[0]
	if sda.Device != "sda" || sda.ReadsDone != 1000 || sda.ReadBytes() != 20000*512 ||
		sda.WriteBytes() != 8000*512 || sda.IOMs != 1500 || sda.IOInProgress != 2 {
		t.Errorf("unexpected sda: %+v", sda)
	}
	if _, err := ParseDiskStats(strings.NewReader("8 0 sda 1 2\n")); err == nil {
		t.Error("expected short-line error")
	}
}

const netFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000      50    0    0    0     0          0         0     5000      50    0    0    0     0       0          0
  eth0: 123456   900    3    4    0     0          0         0    654321     800    1    2    0     0       0          0
`

func TestParseNetDev(t *testing.T) {
	n, err := ParseNetDev(strings.NewReader(netFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 2 {
		t.Fatalf("want 2 interfaces, got %d", len(n))
	}
	e := n[1]
	if e.Interface != "eth0" || e.RxBytes != 123456 || e.RxErrors != 3 || e.RxDropped != 4 ||
		e.TxBytes != 654321 || e.TxPackets != 800 || e.TxErrors != 1 || e.TxDropped != 2 {
		t.Errorf("unexpected eth0: %+v", e)
	}
	if _, err := ParseNetDev(strings.NewReader("eth0: 1 2 3\n")); err == nil {
		t.Error("expected short-line error")
	}
}

func TestFSReadLiveProc(t *testing.T) {
	s, err := FS{Root: "/proc"}.Read()
	if err != nil {
		t.Skipf("live /proc unavailable: %v", err)
	}
	if len(s.CPUs) == 0 || s.Mem.Total == 0 {
		t.Errorf("implausible snapshot: %+v", s)
	}
}
