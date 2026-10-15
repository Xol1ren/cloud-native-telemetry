// Package exporter renders host metrics in the Prometheus text exposition format.
package exporter

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/procfs"
)

// Usage holds derived CPU percentages for one CPU line.
type Usage struct {
	Name      string
	BusyPct   float64
	IOWaitPct float64
}

// State is the data rendered on each scrape.
type State struct {
	Snap  procfs.Snapshot
	Usage []Usage // first element is the aggregate ("cpu")
}

// Exporter stores the latest State and serves it over HTTP.
type Exporter struct {
	mu    sync.RWMutex
	state State
	ready bool

	AnomaliesByKind sync.Map // string -> *atomic.Uint64
	PushFailures    atomic.Uint64
	PushedEvents    atomic.Uint64
}

// New creates an Exporter.
func New() *Exporter { return &Exporter{} }

// Update replaces the stored state.
func (e *Exporter) Update(s State) {
	e.mu.Lock()
	e.state, e.ready = s, true
	e.mu.Unlock()
}

// CountAnomaly increments the anomaly counter for kind.
func (e *Exporter) CountAnomaly(kind string) {
	v, _ := e.AnomaliesByKind.LoadOrStore(kind, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// Handler returns the /metrics handler.
func (e *Exporter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		e.Render(w)
	})
}

func label(name, val string) string { return fmt.Sprintf(`{%s=%q}`, name, val) }

// Render writes all metrics to w.
func (e *Exporter) Render(w io.Writer) {
	e.mu.RLock()
	st, ready := e.state, e.ready
	e.mu.RUnlock()

	head := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	if ready {
		head("cloudpulse_cpu_usage_percent", "gauge", "CPU busy percentage over the last sampling interval.")
		for _, u := range st.Usage {
			fmt.Fprintf(w, "cloudpulse_cpu_usage_percent%s %.4f\n", label("cpu", cpuLabel(u.Name)), u.BusyPct)
		}
		head("cloudpulse_cpu_iowait_percent", "gauge", "CPU iowait percentage over the last sampling interval.")
		for _, u := range st.Usage {
			fmt.Fprintf(w, "cloudpulse_cpu_iowait_percent%s %.4f\n", label("cpu", cpuLabel(u.Name)), u.IOWaitPct)
		}

		m := st.Snap.Mem
		for _, g := range []struct {
			name, help string
			v          uint64
		}{
			{"cloudpulse_memory_total_bytes", "Total physical memory.", m.Total},
			{"cloudpulse_memory_free_bytes", "Free memory.", m.Free},
			{"cloudpulse_memory_available_bytes", "Memory available for new workloads.", m.Available},
			{"cloudpulse_memory_buffers_bytes", "Memory used by kernel buffers.", m.Buffers},
			{"cloudpulse_memory_cached_bytes", "Memory used by the page cache.", m.Cached},
		} {
			head(g.name, "gauge", g.help)
			fmt.Fprintf(w, "%s %d\n", g.name, g.v)
		}
		head("cloudpulse_memory_used_percent", "gauge", "Percentage of memory not available.")
		fmt.Fprintf(w, "cloudpulse_memory_used_percent %.4f\n", m.UsedRatio()*100)

		disks := append([]procfs.DiskStat(nil), st.Snap.Disks...)
		sort.Slice(disks, func(i, j int) bool { return disks[i].Device < disks[j].Device })
		counter := func(name, help string, f func(procfs.DiskStat) string) {
			head(name, "counter", help)
			for _, d := range disks {
				fmt.Fprintf(w, "%s%s %s\n", name, label("device", d.Device), f(d))
			}
		}
		u := func(v uint64) string { return fmt.Sprintf("%d", v) }
		counter("cloudpulse_disk_read_bytes_total", "Bytes read from device.", func(d procfs.DiskStat) string { return u(d.ReadBytes()) })
		counter("cloudpulse_disk_written_bytes_total", "Bytes written to device.", func(d procfs.DiskStat) string { return u(d.WriteBytes()) })
		counter("cloudpulse_disk_reads_completed_total", "Completed read operations.", func(d procfs.DiskStat) string { return u(d.ReadsDone) })
		counter("cloudpulse_disk_writes_completed_total", "Completed write operations.", func(d procfs.DiskStat) string { return u(d.WritesDone) })
		counter("cloudpulse_disk_io_time_seconds_total", "Seconds spent doing I/O.", func(d procfs.DiskStat) string { return fmt.Sprintf("%.3f", float64(d.IOMs)/1000) })
		head("cloudpulse_disk_io_in_progress", "gauge", "I/O operations currently in flight.")
		for _, d := range disks {
			fmt.Fprintf(w, "cloudpulse_disk_io_in_progress%s %d\n", label("device", d.Device), d.IOInProgress)
		}

		nets := append([]procfs.NetStat(nil), st.Snap.Nets...)
		sort.Slice(nets, func(i, j int) bool { return nets[i].Interface < nets[j].Interface })
		ncounter := func(name, help string, f func(procfs.NetStat) uint64) {
			head(name, "counter", help)
			for _, n := range nets {
				fmt.Fprintf(w, "%s%s %d\n", name, label("interface", n.Interface), f(n))
			}
		}
		ncounter("cloudpulse_network_receive_bytes_total", "Bytes received.", func(n procfs.NetStat) uint64 { return n.RxBytes })
		ncounter("cloudpulse_network_transmit_bytes_total", "Bytes transmitted.", func(n procfs.NetStat) uint64 { return n.TxBytes })
		ncounter("cloudpulse_network_receive_packets_total", "Packets received.", func(n procfs.NetStat) uint64 { return n.RxPackets })
		ncounter("cloudpulse_network_transmit_packets_total", "Packets transmitted.", func(n procfs.NetStat) uint64 { return n.TxPackets })
		ncounter("cloudpulse_network_receive_errors_total", "Receive errors.", func(n procfs.NetStat) uint64 { return n.RxErrors })
		ncounter("cloudpulse_network_transmit_errors_total", "Transmit errors.", func(n procfs.NetStat) uint64 { return n.TxErrors })
	}

	head("cloudpulse_anomalies_detected_total", "counter", "Anomalies detected by the agent, by kind.")
	var kinds []string
	e.AnomaliesByKind.Range(func(k, _ any) bool { kinds = append(kinds, k.(string)); return true })
	sort.Strings(kinds)
	for _, k := range kinds {
		v, _ := e.AnomaliesByKind.Load(k)
		fmt.Fprintf(w, "cloudpulse_anomalies_detected_total%s %d\n", label("kind", k), v.(*atomic.Uint64).Load())
	}
	head("cloudpulse_agent_pushed_events_total", "counter", "Anomaly events delivered to the control plane.")
	fmt.Fprintf(w, "cloudpulse_agent_pushed_events_total %d\n", e.PushedEvents.Load())
	head("cloudpulse_agent_push_failures_total", "counter", "Batches dropped after exhausting retries.")
	fmt.Fprintf(w, "cloudpulse_agent_push_failures_total %d\n", e.PushFailures.Load())
}

func cpuLabel(name string) string {
	if name == "cpu" {
		return "all"
	}
	return name
}
