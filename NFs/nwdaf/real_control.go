package main

// The lab endpoint intentionally has no MOS model or XR profile assumptions.
import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"
)

type realSample struct {
	Timestamp       time.Time `json:"timestamp"`
	Interval        float64   `json:"intervalSeconds"`
	Throughput      float64   `json:"throughputMbps"`
	RTT             *float64  `json:"rttMs"`
	Loss            float64   `json:"packetLossPercent"`
	ExpectedPackets uint64    `json:"expectedPackets"`
	LossDelta       int64     `json:"lossDelta"`
}
type realEvaluation struct {
	Now     time.Time    `json:"now"`
	Offered float64      `json:"offeredMbps"`
	Samples []realSample `json:"samples"`
}
type realIndicators struct {
	Throughput float64 `json:"throughputMbps"`
	RTT        float64 `json:"rttMs"`
	Loss       float64 `json:"packetLossPercent"`
}

func (v realIndicators) breached(offered float64) bool {
	return v.Throughput < .95*offered || v.RTT > 20 || v.Loss > 5
}
func realForecast(req realEvaluation) map[string]any {
	out := map[string]any{"source": "real-udp-lab", "valid": false, "forecastReady": false, "horizonSeconds": 5, "minimumSamples": 5}
	latest := time.Time{}
	for _, sample := range req.Samples {
		if !sample.Timestamp.After(req.Now) && sample.Timestamp.After(latest) {
			latest = sample.Timestamp
		}
	}
	if latest.IsZero() || req.Now.Sub(latest) > 6*time.Second {
		return out
	}
	out["windowEnd"] = latest
	var measured realIndicators
	var coverage, expected, lost float64
	recent := []realSample{}
	for _, s := range req.Samples {
		age := req.Now.Sub(s.Timestamp).Seconds()
		if age < 0 || age > 10 || s.Interval <= 0 || s.Interval > 1.5 || s.RTT == nil {
			continue
		}
		recent = append(recent, s)
		dt := math.Max(0, math.Min(s.Interval, 5-latest.Sub(s.Timestamp).Seconds()))
		measured.Throughput += dt * s.Throughput
		measured.RTT += dt * (*s.RTT)
		measured.Loss += dt * s.Loss
		coverage += dt
		expected += float64(s.ExpectedPackets) * dt / s.Interval
		lost += float64(s.LossDelta) * dt / s.Interval
	}
	if coverage < 4.75 || coverage > 5.01 {
		return out
	}
	measured.Throughput /= coverage
	measured.RTT /= coverage
	measured.Loss /= coverage
	if expected > 0 {
		measured.Loss = math.Max(0, 100*lost/expected)
	}
	out["valid"] = true
	out["measured"] = measured
	out["measuredViolation"] = measured.breached(req.Offered)
	if len(recent) < 5 {
		return out
	}
	// Actual elapsed seconds, not the sample-index horizon of legacy analytics.
	n := float64(len(recent))
	var sx, sxx float64
	var sy, sxy realIndicators
	for _, s := range recent {
		x := s.Timestamp.Sub(req.Now).Seconds()
		sx += x
		sxx += x * x
		sy.Throughput += s.Throughput
		sy.RTT += *s.RTT
		sy.Loss += s.Loss
		sxy.Throughput += x * s.Throughput
		sxy.RTT += x * (*s.RTT)
		sxy.Loss += x * s.Loss
	}
	denominator := n*sxx - sx*sx
	if denominator <= 0 {
		return out
	}
	predict := func(y, xy float64) float64 {
		slope := (n*xy - sx*y) / denominator
		return math.Max(0, (y-slope*sx)/n+5*slope)
	}
	predicted := realIndicators{predict(sy.Throughput, sxy.Throughput), predict(sy.RTT, sxy.RTT), math.Min(100, predict(sy.Loss, sxy.Loss))}
	out["forecastReady"] = true
	out["predicted"] = predicted
	out["predictedViolation"] = predicted.breached(req.Offered)
	return out
}
func handleRealEvaluation(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var req realEvaluation
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.Now.IsZero() || req.Offered <= 0 || len(req.Samples) > 600 {
		http.Error(w, "invalid real-traffic evaluation", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(realForecast(req))
}
