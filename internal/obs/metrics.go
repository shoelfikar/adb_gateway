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

	// SessionState is a per-device-serial one-hot encoded gauge introduced in
	// Plan 03-02 (DEV-05). Each (device_serial,state) series reads 1 if the
	// device is currently in that state, 0 otherwise. PromQL consumers can
	// `sum by (state) (gateway_session_state)` to get fleet counts and
	// `gateway_session_state{device_serial="X"} == 1` to find a single
	// device's state. Cardinality bound: ~30 devices x 6 states = 180 series.
	//
	// NOTE: this is the only Phase 3 metric that intentionally adds the
	// `device_serial` label per CONTEXT.md Claude's Discretion (the Phase 2
	// D-18 cardinality lock applies to baseline collectors only — Phase 3
	// per-device gauges are explicitly carved out).
	SessionState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_session_state",
		Help: "Current session state, one-hot encoded by device_serial and state name.",
	}, []string{"device_serial", "state"})
)

// allSessionStateNames lists every defined session-state string. SetSessionState
// uses it to zero non-current series so the one-hot invariant holds. Keep in
// sync with internal/session.AllStates() — we duplicate the names here to
// avoid an import cycle (session already depends on obs).
var allSessionStateNames = []string{
	"idle",
	"starting",
	"active",
	"stopping",
	"failed",
	"reconnecting",
}

// SetSessionState records a device's current session state via the one-hot
// encoded gateway_session_state gauge. All non-current state series for the
// same serial are forced to 0; the current state series is set to 1. The
// Set call is atomic per-series in client_golang, so concurrent calls for
// the SAME serial may briefly observe two series at 1 — this is acceptable
// for a Prometheus gauge (scrape will see at most a 1-tick drift).
//
// Callers should hold the per-device mutex when invoking this so transitions
// are observed in FSM order.
func SetSessionState(serial, current string) {
	for _, s := range allSessionStateNames {
		if s == current {
			continue
		}
		SessionState.WithLabelValues(serial, s).Set(0)
	}
	SessionState.WithLabelValues(serial, current).Set(1)
}

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
		SessionState,
	)
}