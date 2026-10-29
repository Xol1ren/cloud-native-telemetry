// Command collector-agent is a Linux host metrics daemon exposing Prometheus
// metrics and pushing anomaly spikes to the CloudPulse control plane.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/anomaly"
	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/exporter"
	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/procfs"
	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/push"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		slog.Warn("invalid float env, using default", "key", key, "value", v)
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("invalid duration env, using default", "key", key, "value", v)
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	procRoot := env("PROC_ROOT", "/proc")
	listen := env("LISTEN_ADDR", ":9100")
	node, _ := os.Hostname()
	node = env("NODE_NAME", node)
	interval := envDur("SAMPLE_INTERVAL", 1*time.Second)
	cpURL := env("CONTROL_PLANE_URL", "") // e.g. http://control-plane:8000

	th := anomaly.DefaultThresholds()
	th.CPUPercent = envFloat("CPU_THRESHOLD", th.CPUPercent)
	th.MemUsedPct = envFloat("MEM_USED_THRESHOLD", th.MemUsedPct)
	th.IOWaitPercent = envFloat("IOWAIT_THRESHOLD", th.IOWaitPercent)
	th.Cooldown = envDur("ANOMALY_COOLDOWN", th.Cooldown)

	exp := exporter.New()
	det := anomaly.NewDetector(th)
	fs := procfs.FS{Root: procRoot}

	// Capture both SIGINT and SIGTERM for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var pusher *push.Client
	if cpURL != "" {
		pusher = push.New(push.Config{URL: cpURL + "/api/v1/telemetry/ingest", Node: node}, push.Hooks{
			OnDelivered: func(n int) { exp.PushedEvents.Add(uint64(n)) },
			OnDropped:   func(n int) { exp.PushFailures.Add(uint64(n)) },
		})
		go pusher.Run(ctx)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", exp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		slog.Info("collector listening", "addr", listen, "proc", procRoot, "node", node, "push", cpURL != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "err", err)
			stop()
		}
	}()

	var prev, cur procfs.Snapshot
	if err := fs.ReadInto(&prev); err != nil {
		slog.Error("initial proc read failed", "err", err)
		os.Exit(1)
	}

	// Reusable usage slice to avoid allocations during sample loop
	var usage []exporter.Usage
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("initiating graceful shutdown on signal (SIGINT/SIGTERM)")
			stop() // stop catching signals so second signal triggers immediate exit

			// Gracefully drain HTTP server connections
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := srv.Shutdown(sctx); err != nil {
				slog.Error("http server graceful shutdown error", "err", err)
				_ = srv.Close()
			} else {
				slog.Info("http server gracefully stopped")
			}
			cancel()

			// Gracefully drain pending events in anomaly push client
			if pusher != nil {
				slog.Info("draining pending anomaly push batches")
				select {
				case <-pusher.Done():
					slog.Info("anomaly push client drained successfully")
				case <-time.After(3 * time.Second):
					slog.Warn("timeout waiting for anomaly pusher to drain")
				}
			}
			slog.Info("collector agent shutdown complete")
			return

		case now := <-tick.C:
			if err := fs.ReadInto(&cur); err != nil {
				slog.Warn("proc read failed", "err", err)
				continue
			}

			usage = usage[:0]
			for _, c := range cur.CPUs {
				for _, p := range prev.CPUs {
					if p.Name == c.Name {
						b, w := procfs.CPUUsage(p, c)
						usage = append(usage, exporter.Usage{Name: c.Name, BusyPct: b, IOWaitPct: w})
						break
					}
				}
			}
			exp.Update(exporter.State{Snap: cur, Usage: usage})

			if len(usage) > 0 && usage[0].Name == "cpu" {
				events := det.Evaluate(anomaly.Sample{
					Time: now, CPUPercent: usage[0].BusyPct, IOWaitPct: usage[0].IOWaitPct,
					MemUsedPct: cur.Mem.UsedRatio() * 100, MemAvailable: cur.Mem.Available, MemTotal: cur.Mem.Total,
				})
				for _, e := range events {
					exp.CountAnomaly(e.Kind)
					slog.Warn("anomaly detected", "kind", e.Kind, "value", e.Value, "threshold", e.Threshold)
					if pusher != nil {
						pusher.Enqueue(e)
					}
				}
			}

			// Swap snapshots to retain capacity in next iteration
			prev, cur = cur, prev
		}
	}
}
