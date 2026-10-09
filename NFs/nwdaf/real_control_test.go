package main

import (
	"testing"
	"time"
)

func TestRealForecastUsesSecondsAndRejectsMissingData(t *testing.T) {
	now := time.Unix(1000, 0)
	req := realEvaluation{Now: now, Offered: 22}
	// RTT increases one millisecond per second; its five-second-ahead value is 24.
	for i := 9; i >= 0; i-- {
		rtt := 19 - float64(i)
		req.Samples = append(req.Samples, realSample{Timestamp: now.Add(-time.Duration(i) * time.Second), Interval: 1, Throughput: 22, RTT: &rtt})
	}
	got := realForecast(req)
	if got["valid"] != true || got["measuredViolation"] != false || got["predictedViolation"] != true {
		t.Fatalf("unexpected forecast: %#v", got)
	}
	// Sampling phase does not turn a complete five-second measured window missing.
	req.Now = now.Add(800 * time.Millisecond)
	if realForecast(req)["valid"] != true {
		t.Fatal("rejected a fresh complete window")
	}
	req.Now = now.Add(7 * time.Second)
	if realForecast(req)["valid"] != false {
		t.Fatal("stale samples accepted")
	}
	req.Now = now
	req.Samples = req.Samples[:3]
	if realForecast(req)["valid"] != false {
		t.Fatal("incomplete coverage accepted")
	}
}
