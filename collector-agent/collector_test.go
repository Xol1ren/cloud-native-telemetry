package main

import (
	"strings"
	"testing"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/procfs"
)

const benchStatData = `cpu  100 10 50 800 20 5 5 10 0 0
cpu0 50 5 25 400 10 2 3 5 0 0
cpu1 50 5 25 400 10 3 2 5 0 0
intr 12345
ctxt 999
`

const benchMemData = `MemTotal:       16000000 kB
MemFree:         2000000 kB
MemAvailable:    8000000 kB
Buffers:          500000 kB
Cached:          4000000 kB
SwapTotal:       2000000 kB
SwapFree:        1000000 kB
Dirty:                 8 kB
`

func BenchmarkParseProcStat(b *testing.B) {
	b.ReportAllocs()
	r := strings.NewReader(benchStatData)
	var dst []procfs.CPUStat
	for i := 0; i < b.N; i++ {
		r.Reset(benchStatData)
		stats, err := procfs.ParseStatInto(r, dst)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		dst = stats
	}
}

func BenchmarkParseProcMeminfo(b *testing.B) {
	b.ReportAllocs()
	r := strings.NewReader(benchMemData)
	for i := 0; i < b.N; i++ {
		r.Reset(benchMemData)
		m, err := procfs.ParseMemInfo(r)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if m.Total == 0 {
			b.Fatal("expected non-zero MemTotal")
		}
	}
}

func TestCollectorAgentParsers(t *testing.T) {
	rStat := strings.NewReader(benchStatData)
	cpus, err := procfs.ParseStat(rStat)
	if err != nil {
		t.Fatalf("ParseStat failed: %v", err)
	}
	if len(cpus) != 3 {
		t.Fatalf("expected 3 cpus, got %d", len(cpus))
	}
	procfs.PutCPUStats(cpus)

	rMem := strings.NewReader(benchMemData)
	mem, err := procfs.ParseMemInfo(rMem)
	if err != nil {
		t.Fatalf("ParseMemInfo failed: %v", err)
	}
	if mem.Total == 0 || mem.Available == 0 {
		t.Fatalf("invalid meminfo parsed: %+v", mem)
	}
}
