package anomaly

import (
	"testing"
	"time"
)

func TestEvaluateThresholds(t *testing.T) {
	d := NewDetector(DefaultThresholds())
	now := time.Unix(1000, 0)

	if ev := d.Evaluate(Sample{Time: now, CPUPercent: 85, IOWaitPct: 20, MemUsedPct: 90}); len(ev) != 0 {
		t.Fatalf("values at threshold must not fire, got %v", ev)
	}
	ev := d.Evaluate(Sample{Time: now, CPUPercent: 95, IOWaitPct: 40, MemUsedPct: 97, MemTotal: 100, MemAvailable: 3})
	kinds := map[string]bool{}
	for _, e := range ev {
		kinds[e.Kind] = true
	}
	for _, k := range []string{KindCPUSpike, KindIOWait, KindMemoryPressure} {
		if !kinds[k] {
			t.Errorf("missing %s in %v", k, ev)
		}
	}
}

func TestCooldown(t *testing.T) {
	d := NewDetector(DefaultThresholds())
	now := time.Unix(1000, 0)
	s := Sample{Time: now, CPUPercent: 99}
	if len(d.Evaluate(s)) != 1 {
		t.Fatal("first spike should fire")
	}
	s.Time = now.Add(10 * time.Second)
	if len(d.Evaluate(s)) != 0 {
		t.Fatal("spike within cooldown should be suppressed")
	}
	s.Time = now.Add(61 * time.Second)
	if len(d.Evaluate(s)) != 1 {
		t.Fatal("spike after cooldown should fire")
	}
}
