package main

import (
	"math"
	"testing"
	"time"
)

func TestAnalyticsPreservesInputTimestampWhenQueriedAgain(t *testing.T) {
	engine := NewAnalyticsEngine()
	old := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	engine.IngestMetrics([]VRMetrics{{SessionID: "stale-session", Timestamp: old, Profile: "INTERACTIVE_VR", Throughput: 150, Latency: 5}})
	for i := 0; i < 2; i++ {
		result := engine.ComputeAnalytics("SERVICE_EXPERIENCE")
		if len(result.SessionAnalytics) != 1 || result.SessionAnalytics[0].LastSampleAt != old {
			t.Fatalf("query refreshed the input timestamp: %+v", result.SessionAnalytics)
		}
	}
	newest := time.Now().UTC().Format(time.RFC3339Nano)
	engine.IngestMetrics([]VRMetrics{{SessionID: "stale-session", Timestamp: newest, Profile: "INTERACTIVE_VR", Throughput: 150, Latency: 5}})
	if got := engine.ComputeAnalytics("SERVICE_EXPERIENCE").SessionAnalytics[0].LastSampleAt; got != newest {
		t.Fatalf("new sample not reflected: %s", got)
	}
}

func TestCalculateMOSIsProfileRelative(t *testing.T) {
	video := CalculateMOS("360_VIDEO", 100, 5, 1, 0)
	interactive := CalculateMOS("INTERACTIVE_VR", 100, 5, 1, 0)
	gaming := CalculateMOS("CLOUD_GAMING", 100, 5, 1, 0)

	if !(video > interactive && interactive > gaming) {
		t.Fatalf("expected profile-relative ordering, got video=%.3f interactive=%.3f gaming=%.3f", video, interactive, gaming)
	}
}

func TestFixedImpairmentRecoveryProducesMOSRebound(t *testing.T) {
	before := CalculateMOS("CLOUD_GAMING", 300, 45, 0.5, 5)
	after := CalculateMOS("INTERACTIVE_VR", 150, 11, 1, 0.75)
	if after-before < 0.5 {
		t.Fatalf("expected at least +0.5 MOS rebound, before=%.3f after=%.3f", before, after)
	}
	if after < 3.0 {
		t.Fatalf("recovered MOS remains below SLA: %.3f", after)
	}
}

func TestCalculateMOSUsesDocumentedFallback(t *testing.T) {
	got := CalculateMOS("UNKNOWN", 200, 5, 1, 0)
	want := CalculateMOS("INTERACTIVE_VR", 150, 5, 1, 0)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("fallback target mismatch: got %.6f want %.6f", got, want)
	}
}

func TestCurrentProfileWindowUsesNewestContiguousSegment(t *testing.T) {
	metrics := []VRMetrics{
		{Profile: "CLOUD_GAMING", Throughput: 300},
		{Profile: "CLOUD_GAMING", Throughput: 310},
		{Profile: "INTERACTIVE_VR", Throughput: 140},
		{Profile: "INTERACTIVE_VR", Throughput: 150},
	}

	got := currentProfileWindow(metrics, 50)
	if len(got) != 2 {
		t.Fatalf("expected two current-profile samples, got %d", len(got))
	}
	for _, metric := range got {
		if metric.Profile != "INTERACTIVE_VR" {
			t.Fatalf("window contains previous profile: %+v", got)
		}
	}
}

func TestComputeAnalyticsResetsWindowAtProfileChange(t *testing.T) {
	engine := NewAnalyticsEngine()
	for i := 0; i < 50; i++ {
		engine.IngestMetrics([]VRMetrics{{
			SessionID: "session-1", Profile: "CLOUD_GAMING",
			Throughput: 300, Latency: 45, Jitter: 1, PacketLoss: 5,
		}})
	}
	engine.IngestMetrics([]VRMetrics{{
		SessionID: "session-1", Profile: "INTERACTIVE_VR",
		Throughput: 150, Latency: 5, Jitter: 1, PacketLoss: 0,
	}})

	result := engine.ComputeAnalytics("SERVICE_EXPERIENCE")
	if result.MOSModelVersion != MOSModelVersion {
		t.Fatalf("unexpected MOS model version %q", result.MOSModelVersion)
	}
	if len(result.SessionAnalytics) != 1 {
		t.Fatalf("expected one session, got %d", len(result.SessionAnalytics))
	}
	session := result.SessionAnalytics[0]
	if session.Profile != "INTERACTIVE_VR" || session.ProfileWindowSampleCount != 1 {
		t.Fatalf("profile window was not reset: %+v", session)
	}
	if session.MOSScore < 4.5 {
		t.Fatalf("old impaired samples diluted post-change MOS: %.2f", session.MOSScore)
	}
	if session.ForecastReady {
		t.Fatal("a new profile must collect five samples before its slope is ready")
	}
}

func TestAnalyticsExposesForecastEvidenceAndTargetFailures(t *testing.T) {
	engine := NewAnalyticsEngine()
	for i := 0; i < 5; i++ {
		engine.IngestMetrics([]VRMetrics{{SessionID: "vr", Profile: "INTERACTIVE_VR",
			Throughput: 170 - float64(i), Latency: 10 + float64(i), PacketLoss: .1, Jitter: 1}})
	}
	result := engine.ComputeAnalytics("SERVICE_EXPERIENCE")
	forecast := result.Forecast
	if forecast.HorizonSamples != 10 || forecast.NominalHorizonSec != 5 || forecast.MinSamples != 5 {
		t.Fatalf("missing sample-based horizon: %+v", forecast)
	}
	s := result.SessionAnalytics[0]
	if !s.ForecastReady || s.QoSSustained {
		t.Fatalf("expected a ready forecast failing its latency target: %+v", s)
	}
	checks := s.QoSTargets
	if checks.Throughput.Predicted != 156 || checks.Throughput.Target != 150 || !checks.Throughput.Met {
		t.Fatalf("unexpected throughput evidence: %+v", checks.Throughput)
	}
	if checks.Latency.Predicted != 24 || checks.Latency.Target != 20 || checks.Latency.Met {
		t.Fatalf("unexpected latency evidence: %+v", checks.Latency)
	}
	if checks.PacketLoss.Target != .3 || !checks.PacketLoss.Met {
		t.Fatalf("unexpected packet loss target: %+v", checks.PacketLoss)
	}
	if s.PredictedMOS != math.Round(CalculateMOS("INTERACTIVE_VR", 156, 24, 1, .1)*100)/100 {
		t.Fatalf("forecast MOS disagrees with exposed components: %+v", s)
	}
}

func TestQoSTargetEqualityAndWarmup(t *testing.T) {
	for _, test := range []struct {
		profile                   string
		throughput, latency, loss float64
	}{{"CLOUD_GAMING", 300, 15, .1}, {"INTERACTIVE_VR", 150, 20, .3}, {"360_VIDEO", 75, 25, .5}} {
		t.Run(test.profile, func(t *testing.T) {
			engine := NewAnalyticsEngine()
			for count := 1; count <= 5; count++ {
				engine.IngestMetrics([]VRMetrics{{SessionID: "vr", Profile: test.profile,
					Throughput: test.throughput, Latency: test.latency, PacketLoss: test.loss}})
				s := engine.ComputeAnalytics("SERVICE_EXPERIENCE").SessionAnalytics[0]
				if s.ForecastReady != (count >= 5) || !s.QoSSustained || !s.QoSTargets.Throughput.Met || !s.QoSTargets.Latency.Met || !s.QoSTargets.PacketLoss.Met {
					t.Fatalf("incorrect readiness or inclusive target bounds with %d samples: %+v", count, s)
				}
			}
		})
	}
}
