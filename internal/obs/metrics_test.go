package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		for _, m := range fam.Metric {
			for _, lp := range m.Label {
				require.False(t, forbidden[lp.GetName()],
					"forbidden label %q on metric %s", lp.GetName(), fam.GetName())
			}
		}
	}
}