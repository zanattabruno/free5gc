package main

import (
	"math"
	"testing"
)

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
}
