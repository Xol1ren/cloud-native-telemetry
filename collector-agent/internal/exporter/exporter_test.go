package exporter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/procfs"
)

func TestRender(t *testing.T) {
	e := New()
	var empty bytes.Buffer
	e.Render(&empty)
	if strings.Contains(empty.String(), "cloudpulse_cpu_usage_percent") {
		t.Error("cpu metrics should not render before first update")
	}

	e.Update(State{
		Usage: []Usage{{Name: "cpu", BusyPct: 12.5, IOWaitPct: 1}, {Name: "cpu0", BusyPct: 20}},
		Snap: procfs.Snapshot{
			Mem:   procfs.MemInfo{Total: 1000, Available: 250, Free: 100, Buffers: 50, Cached: 100},
			Disks: []procfs.DiskStat{{Device: "sda", SectorsRead: 2, IOMs: 1500}},
			Nets:  []procfs.NetStat{{Interface: "eth0", RxBytes: 42}},
		},
	})
	e.CountAnomaly("cpu_spike")
	e.CountAnomaly("cpu_spike")

	var buf bytes.Buffer
	e.Render(&buf)
	out := buf.String()
	for _, want := range []string{
		"# TYPE cloudpulse_cpu_usage_percent gauge",
		`cloudpulse_cpu_usage_percent{cpu="all"} 12.5000`,
		`cloudpulse_cpu_usage_percent{cpu="cpu0"} 20.0000`,
		"cloudpulse_memory_available_bytes 250",
		"cloudpulse_memory_used_percent 75.0000",
		`cloudpulse_disk_read_bytes_total{device="sda"} 1024`,
		`cloudpulse_disk_io_time_seconds_total{device="sda"} 1.500`,
		`cloudpulse_network_receive_bytes_total{interface="eth0"} 42`,
		`cloudpulse_anomalies_detected_total{kind="cpu_spike"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}
