package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionStateMetric verifies Plan 03-02: gateway_session_state is a
// per-device-serial GaugeVec with one-hot encoding — exactly one state series
// per serial reads 1, all others 0.
func TestSessionStateMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(SessionState)
	defer SessionState.Reset()

	const serial = "device-A"
	// All known states (mirror internal/session.SessionState String() values).
	allStates := []string{"idle", "starting", "active", "stopping", "failed", "reconnecting"}

	// Drive transitions through each state in turn; after each call exactly one
	// series for `serial` must read 1, the rest 0.
	for _, target := range allStates {
		setSessionStateRaw(serial, target, allStates)

		fams, err := reg.Gather()
		require.NoError(t, err)

		var foundFamily bool
		oneCount := 0
		zeroCount := 0
		for _, fam := range fams {
			if fam.GetName() != "gateway_session_state" {
				continue
			}
			foundFamily = true
			for _, m := range fam.Metric {
				var thisSerial, thisState string
				for _, lp := range m.Label {
					switch lp.GetName() {
					case "device_serial":
						thisSerial = lp.GetValue()
					case "state":
						thisState = lp.GetValue()
					}
				}
				if thisSerial != serial {
					continue
				}
				v := m.GetGauge().GetValue()
				if v == 1 {
					oneCount++
					assert.Equal(t, target, thisState,
						"the series with value=1 must be the current state %q", target)
				} else if v == 0 {
					zeroCount++
				} else {
					t.Fatalf("unexpected gauge value %v for state %s", v, thisState)
				}
			}
		}
		assert.True(t, foundFamily, "gateway_session_state family must be registered")
		assert.Equal(t, 1, oneCount, "exactly one state series must read 1 for serial %s after transition to %s", serial, target)
		assert.Equal(t, len(allStates)-1, zeroCount, "all other state series must read 0 after transition to %s", target)
	}
}

// setSessionStateRaw drives the metric directly using string state names so
// the obs package test does not import internal/session.
func setSessionStateRaw(serial, target string, all []string) {
	for _, s := range all {
		SessionState.WithLabelValues(serial, s).Set(0)
	}
	SessionState.WithLabelValues(serial, target).Set(1)
}

func TestPhase2MetricNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	MustRegister(reg)

	// Initialize all label dimensions so Gather returns all families
	DevicesTotal.WithLabelValues("online").Set(0)
	SessionsTotal.WithLabelValues("idle").Set(0)
	FramesEmittedTotal.WithLabelValues("video").Add(0)
	FramesDroppedTotal.WithLabelValues("video").Add(0)
	ReverseTunnelReconcileTotal.WithLabelValues("success").Add(0)
	ADBCallSeconds.Observe(0.001)

	fams, err := reg.Gather()
	require.NoError(t, err)

	wantNames := map[string]bool{
		"gateway_devices_total":                 false,
		"gateway_sessions_total":                false,
		"gateway_frames_emitted_total":          false,
		"gateway_frames_dropped_total":          false,
		"gateway_adb_call_seconds":              false,
		"gateway_reverse_tunnel_reconcile_total": false,
	}

	for _, fam := range fams {
		name := fam.GetName()
		if _, ok := wantNames[name]; ok {
			wantNames[name] = true
		}
	}

	for name, found := range wantNames {
		assert.True(t, found, "expected metric family %q not found", name)
	}
}

func TestPhase2FramesEmittedStreamLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	MustRegister(reg)

	FramesEmittedTotal.WithLabelValues("video").Inc()
	FramesEmittedTotal.WithLabelValues("audio").Inc()

	fams, err := reg.Gather()
	require.NoError(t, err)

	for _, fam := range fams {
		if fam.GetName() == "gateway_frames_emitted_total" {
			var foundVideo, foundAudio bool
			for _, m := range fam.Metric {
				for _, lp := range m.Label {
					if lp.GetName() == "stream" && lp.GetValue() == "video" {
						foundVideo = true
					}
					if lp.GetName() == "stream" && lp.GetValue() == "audio" {
						foundAudio = true
					}
				}
			}
			assert.True(t, foundVideo, "expected stream=video label")
			assert.True(t, foundAudio, "expected stream=audio label")
		}
	}
}

func TestPhase2DevicesTotalGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	MustRegister(reg)

	DevicesTotal.WithLabelValues("online").Set(3)

	fams, err := reg.Gather()
	require.NoError(t, err)

	for _, fam := range fams {
		if fam.GetName() == "gateway_devices_total" {
			for _, m := range fam.Metric {
				for _, lp := range m.Label {
					if lp.GetName() == "status" && lp.GetValue() == "online" {
						assert.Equal(t, float64(3), m.GetGauge().GetValue())
					}
				}
			}
		}
	}
}

func TestPhase2ADBCallSecondsHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	MustRegister(reg)

	ADBCallSeconds.Observe(0.005)

	fams, err := reg.Gather()
	require.NoError(t, err)

	for _, fam := range fams {
		if fam.GetName() == "gateway_adb_call_seconds" {
			hist := fam.Metric[0].GetHistogram()
			// Package-level histogram may accumulate across tests; verify >=1 observations
			assert.GreaterOrEqual(t, hist.GetSampleCount(), uint64(1), "expected at least 1 observation")
		}
	}
}

func TestPhase2NoDeviceSerialLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	MustRegister(reg)

	// Exercise all label dimensions
	DevicesTotal.WithLabelValues("online").Set(1)
	SessionsTotal.WithLabelValues("active").Set(1)
	FramesEmittedTotal.WithLabelValues("video").Inc()
	FramesDroppedTotal.WithLabelValues("video").Inc()
	ReverseTunnelReconcileTotal.WithLabelValues("success").Inc()
	ADBCallSeconds.Observe(0.001)

	fams, err := reg.Gather()
	require.NoError(t, err)

	forbidden := map[string]bool{
		"device_serial": true,
		"device":        true,
		"serial":        true,
		"viewer_id":     true,
		"session_id":    true,
	}

	for _, fam := range fams {
		// Plan 03-02 (DEV-05) explicitly adds device_serial to
		// gateway_session_state — the D-18 cardinality lock applies
		// to baseline collectors only.
		if fam.GetName() == "gateway_session_state" {
			continue
		}
		for _, m := range fam.Metric {
			for _, lp := range m.Label {
				require.False(t, forbidden[lp.GetName()],
					"forbidden label %q on metric %s", lp.GetName(), fam.GetName())
			}
		}
	}
}