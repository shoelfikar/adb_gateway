// Package obs exposes the gateway's structured logging (logging.go) and
// Prometheus metrics surface (metrics.go). Phase 2 baseline collectors do
// NOT use a device_serial label per D-18 (cardinality lock — Phase 3 will
// layer per-device labels onto these same collectors).
package obs

import "github.com/prometheus/client_golang/prometheus"

var (
	DevicesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_devices_total",
		Help: "Number of tracked devices by registry status (online, offline, etc).",
	}, []string{"status"})

	SessionsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_sessions_total",
		Help: "Number of device sessions by FSM state (idle, starting, active, stopping, failed).",
	}, []string{"state"})

	FramesEmittedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_frames_emitted_total",
		Help: "Total frames successfully sent to viewers, by stream type.",
	}, []string{"stream"})

	FramesDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_frames_dropped_total",
		Help: "Total frames dropped due to slow viewer back-pressure, by stream type.",
	}, []string{"stream"})

	ADBCallSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_adb_call_seconds",
		Help:    "ADB protocol call latency in seconds.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms .. ~4s
	})

	ReverseTunnelReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_reverse_tunnel_reconcile_total",
		Help: "Reverse-tunnel reconcile attempts after ADB reconnect, by result.",
	}, []string{"result"})
)

// MustRegister registers all Phase 2 baseline collectors on the provided
// Registerer. Panics if any collector is already registered (matches
// prometheus.MustRegister semantics).
func MustRegister(r prometheus.Registerer) {
	r.MustRegister(
		DevicesTotal,
		SessionsTotal,
		FramesEmittedTotal,
		FramesDroppedTotal,
		ADBCallSeconds,
		ReverseTunnelReconcileTotal,
	)
}