// Package anomaly detects critical resource spikes from derived host metrics.
package anomaly

import "time"

// Event kinds understood by the control plane.
const (
	KindCPUSpike       = "cpu_spike"
	KindMemoryPressure = "memory_pressure"
	KindIOWait         = "io_wait"
)

// Event is a single anomaly occurrence, serialised to the control plane.
type Event struct {
	Kind      string             `json:"kind"`
	Value     float64            `json:"value"`
	Threshold float64            `json:"threshold"`
	Timestamp time.Time          `json:"timestamp"`
	Details   map[string]float64 `json:"details,omitempty"`
}

// Thresholds configures the detector.
type Thresholds struct {
	CPUPercent    float64 // aggregate busy % above which cpu_spike fires
	MemUsedPct    float64 // used % (1 - available/total) above which memory_pressure fires
	IOWaitPercent float64 // aggregate iowait % above which io_wait fires
	Cooldown      time.Duration
}

// DefaultThresholds returns the platform defaults (CPU > 85%, iowait > 20%, mem > 90% used).
func DefaultThresholds() Thresholds {
	return Thresholds{CPUPercent: 85, MemUsedPct: 90, IOWaitPercent: 20, Cooldown: time.Minute}
}

// Sample is the derived input to the detector.
type Sample struct {
	Time         time.Time
	CPUPercent   float64
	IOWaitPct    float64
	MemUsedPct   float64
	MemAvailable uint64
	MemTotal     uint64
}

// Detector evaluates samples with a per-kind cooldown to avoid alert storms.
type Detector struct {
	t    Thresholds
	last map[string]time.Time
}

// NewDetector builds a Detector.
func NewDetector(t Thresholds) *Detector {
	return &Detector{t: t, last: map[string]time.Time{}}
}

// Evaluate returns anomaly events triggered by s. It is not safe for concurrent use.
func (d *Detector) Evaluate(s Sample) []Event {
	var out []Event
	check := func(kind string, value, threshold float64, details map[string]float64) {
		if value <= threshold {
			return
		}
		if prev, ok := d.last[kind]; ok && s.Time.Sub(prev) < d.t.Cooldown {
			return
		}
		d.last[kind] = s.Time
		out = append(out, Event{Kind: kind, Value: value, Threshold: threshold, Timestamp: s.Time, Details: details})
	}
	check(KindCPUSpike, s.CPUPercent, d.t.CPUPercent, nil)
	check(KindMemoryPressure, s.MemUsedPct, d.t.MemUsedPct, map[string]float64{
		"available_bytes": float64(s.MemAvailable),
		"total_bytes":     float64(s.MemTotal),
	})
	check(KindIOWait, s.IOWaitPct, d.t.IOWaitPercent, nil)
	return out
}
