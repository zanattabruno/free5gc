package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Logging (free5gc format) ───────────────────────────────────────────────

func nfLog(level, category, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s [%s][NWDAF][%s] %s\n", time.Now().Format("2006-01-02T15:04:05.000000000-07:00"), level, category, msg)
}

func logInfo(cat, format string, args ...interface{})  { nfLog("INFO", cat, format, args...) }
func logWarn(cat, format string, args ...interface{})  { nfLog("WARN", cat, format, args...) }
func logError(cat, format string, args ...interface{}) { nfLog("ERROR", cat, format, args...) }

// ── Configuration ──────────────────────────────────────────────────────────

type Config struct {
	BindAddr   string
	NrfUri     string
	NfInstID   string
	RegisterIP string
}

var cfg = Config{
	BindAddr:   "127.0.0.15:8000",
	NrfUri:     "http://127.0.0.10:8000",
	NfInstID:   uuid.New().String(),
	RegisterIP: "127.0.0.15",
}

// ── Data Models ────────────────────────────────────────────────────────────

type VRMetrics struct {
	SessionID  string  `json:"sessionId"`
	Timestamp  string  `json:"timestamp"`
	Profile    string  `json:"profile"`
	Throughput float64 `json:"throughputMbps"`
	Latency    float64 `json:"latencyMs"`
	Jitter     float64 `json:"jitterMs"`
	PacketLoss float64 `json:"packetLossPercent"`
	FrameRate  float64 `json:"frameRate"`
}

type MetricsBatch struct {
	Metrics []VRMetrics `json:"metrics"`
}

type SessionAnalytics struct {
	SessionID     string  `json:"sessionId"`
	Profile       string  `json:"profile"`
	AvgThroughput float64 `json:"avgThroughputMbps"`
	AvgLatency    float64 `json:"avgLatencyMs"`
	AvgJitter     float64 `json:"avgJitterMs"`
	AvgPacketLoss float64 `json:"avgPacketLossPercent"`
	MOSScore      float64 `json:"mosScore"`
	SampleCount   int     `json:"sampleCount"`
	PredictedMOS  float64 `json:"predictedMOS"`
	QoSSustained  bool    `json:"qosSustained"`
}

type AnalyticsResult struct {
	EventType           string             `json:"eventType"`
	Timestamp           string             `json:"timestamp"`
	OverallMOS          float64            `json:"overallMOS"`
	OverallPredictedMOS float64            `json:"overallPredictedMOS"`
	AvgThroughput       float64            `json:"avgThroughputMbps"`
	AvgLatency          float64            `json:"avgLatencyMs"`
	AvgJitter           float64            `json:"avgJitterMs"`
	AvgPacketLoss       float64            `json:"avgPacketLossPercent"`
	QoSSustainability   float64            `json:"qosSustainability"`
	AnomalyDetected     bool               `json:"anomalyDetected"`
	AnomalyDetails      string             `json:"anomalyDetails,omitempty"`
	CongestionLevel     float64            `json:"congestionLevel"`
	SessionAnalytics    []SessionAnalytics `json:"sessionAnalytics"`
}

type EventSubscription struct {
	SubscriptionID string   `json:"subscriptionId"`
	EventTypes     []string `json:"eventTypes"`
	NotifURI       string   `json:"notifUri"`
	NotifID        string   `json:"notifId"`
}

type EventNotification struct {
	NotifID   string          `json:"notifId"`
	EventType string          `json:"eventType"`
	Analytics AnalyticsResult `json:"analytics"`
}

// ── Analytics Engine ───────────────────────────────────────────────────────

type AnalyticsEngine struct {
	mu            sync.RWMutex
	metrics       map[string][]VRMetrics // sessionID -> recent metrics
	lastSeen      map[string]time.Time   // sessionID -> last data received
	subscriptions map[string]*EventSubscription
	maxRetention  int
	sessionTTL    time.Duration
}

func NewAnalyticsEngine() *AnalyticsEngine {
	return &AnalyticsEngine{
		metrics:       make(map[string][]VRMetrics),
		lastSeen:      make(map[string]time.Time),
		subscriptions: make(map[string]*EventSubscription),
		maxRetention:  600,
		sessionTTL:    10 * time.Second, // expire sessions with no data for 10s
	}
}

func (e *AnalyticsEngine) IngestMetrics(batch []VRMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for _, m := range batch {
		e.metrics[m.SessionID] = append(e.metrics[m.SessionID], m)
		if len(e.metrics[m.SessionID]) > e.maxRetention {
			e.metrics[m.SessionID] = e.metrics[m.SessionID][len(e.metrics[m.SessionID])-e.maxRetention:]
		}
		e.lastSeen[m.SessionID] = now
	}
}

// StartCleaner removes sessions that haven't received data within the TTL.
func (e *AnalyticsEngine) StartCleaner() {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			e.mu.Lock()
			now := time.Now()
			for sid, last := range e.lastSeen {
				if now.Sub(last) > e.sessionTTL {
					delete(e.metrics, sid)
					delete(e.lastSeen, sid)
					logInfo("Analytics", "Session expired (no data for %s): %s", e.sessionTTL, sid)
				}
			}
			e.mu.Unlock()
		}
	}()
}

func (e *AnalyticsEngine) RemoveSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.metrics, sessionID)
	delete(e.lastSeen, sessionID)
}

// CalculateMOS computes a VR-specific MOS score (1.0-5.0)
func CalculateMOS(throughput, latency, jitter, packetLoss float64) float64 {
	// Throughput factor: VR needs >50 Mbps, ideal >200 Mbps
	tFactor := math.Min(throughput/200.0, 1.0)
	// Latency factor: VR needs <20ms, ideal <5ms
	lFactor := math.Max(0, 1.0-latency/30.0)
	// Jitter factor: VR needs <5ms, ideal <1ms
	jFactor := math.Max(0, 1.0-jitter/10.0)
	// Packet loss factor: VR needs <0.1%, ideal 0%
	pFactor := math.Max(0, 1.0-packetLoss/1.0)

	raw := 0.35*tFactor + 0.30*lFactor + 0.20*jFactor + 0.15*pFactor
	return 1.0 + 4.0*raw // Scale to 1-5
}

// linearRegressionSlope computes the slope of the data points using linear regression
func linearRegressionSlope(data []float64) float64 {
	n := float64(len(data))
	if n < 5 {
		return 0.0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, y := range data {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0.0
	}
	return (n*sumXY - sumX*sumY) / denom
}

func (e *AnalyticsEngine) ComputeAnalytics(eventType string) *AnalyticsResult {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := &AnalyticsResult{
		EventType: eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	var totalT, totalL, totalJ, totalP float64
	var totalMOS, totalPredMOS float64
	sustainedCount := 0
	sessionCount := 0

	for sid, metrics := range e.metrics {
		if len(metrics) == 0 {
			continue
		}
		// Use last 50 samples (or fewer)
		start := 0
		if len(metrics) > 50 {
			start = len(metrics) - 50
		}
		recent := metrics[start:]

		var sT, sL, sJ, sP float64
		throughputs := make([]float64, len(recent))
		latencies := make([]float64, len(recent))
		jitters := make([]float64, len(recent))
		packetLosses := make([]float64, len(recent))

		for i, m := range recent {
			sT += m.Throughput
			sL += m.Latency
			sJ += m.Jitter
			sP += m.PacketLoss

			throughputs[i] = m.Throughput
			latencies[i] = m.Latency
			jitters[i] = m.Jitter
			packetLosses[i] = m.PacketLoss
		}
		n := float64(len(recent))
		avgT := sT / n
		avgL := sL / n
		avgJ := sJ / n
		avgP := sP / n
		mos := CalculateMOS(avgT, avgL, avgJ, avgP)

		// Option B: Predictive Analytics
		slopeT := linearRegressionSlope(throughputs)
		slopeL := linearRegressionSlope(latencies)
		slopeJ := linearRegressionSlope(jitters)
		slopeP := linearRegressionSlope(packetLosses)

		// Predict 10 steps (5 seconds) into the future
		kSteps := 10.0
		predT := math.Max(0, throughputs[len(throughputs)-1]+slopeT*kSteps)
		predL := math.Max(0, latencies[len(latencies)-1]+slopeL*kSteps)
		predJ := math.Max(0, jitters[len(jitters)-1]+slopeJ*kSteps)
		predP := math.Max(0, math.Min(100, packetLosses[len(packetLosses)-1]+slopeP*kSteps))

		predMOS := CalculateMOS(predT, predL, predJ, predP)

		// 3GPP QoS Target definitions for Sustainability check
		var targetT, targetL, targetP float64
		prof := recent[len(recent)-1].Profile
		switch prof {
		case "CLOUD_GAMING":
			targetT = 300.0
			targetL = 15.0
			targetP = 0.1
		case "INTERACTIVE_VR":
			targetT = 150.0
			targetL = 20.0
			targetP = 0.3
		default: // 360_VIDEO
			targetT = 75.0
			targetL = 25.0
			targetP = 0.5
		}

		qosSustained := predT >= targetT && predL <= targetL && predP <= targetP
		if qosSustained {
			sustainedCount++
		}

		sa := SessionAnalytics{
			SessionID:     sid,
			Profile:       prof,
			AvgThroughput: math.Round(avgT*100) / 100,
			AvgLatency:    math.Round(avgL*100) / 100,
			AvgJitter:     math.Round(avgJ*100) / 100,
			AvgPacketLoss: math.Round(avgP*1000) / 1000,
			MOSScore:      math.Round(mos*100) / 100,
			SampleCount:   len(recent),
			PredictedMOS:  math.Round(predMOS*100) / 100,
			QoSSustained:  qosSustained,
		}
		result.SessionAnalytics = append(result.SessionAnalytics, sa)

		totalT += avgT
		totalL += avgL
		totalJ += avgJ
		totalP += avgP
		totalMOS += mos
		totalPredMOS += predMOS
		sessionCount++
	}

	if sessionCount > 0 {
		n := float64(sessionCount)
		result.AvgThroughput = math.Round(totalT/n*100) / 100
		result.AvgLatency = math.Round(totalL/n*100) / 100
		result.AvgJitter = math.Round(totalJ/n*100) / 100
		result.AvgPacketLoss = math.Round(totalP/n*1000) / 1000
		result.OverallMOS = math.Round(totalMOS/n*100) / 100
		result.OverallPredictedMOS = math.Round(totalPredMOS/n*100) / 100
		result.QoSSustainability = math.Round((float64(sustainedCount)/n)*100.0*10) / 10
		result.CongestionLevel = math.Round(math.Max(0, math.Min(100, result.AvgPacketLoss*20+result.AvgLatency/2))*10) / 10

		// Anomaly detection: latency spike/packet loss OR prediction warning
		if result.AvgLatency > 20 || result.AvgPacketLoss > 1.0 {
			result.AnomalyDetected = true
			result.AnomalyDetails = fmt.Sprintf("High latency (%.1fms) or packet loss (%.2f%%)", result.AvgLatency, result.AvgPacketLoss)
		} else if result.OverallPredictedMOS < 3.0 || result.QoSSustainability < 70.0 {
			result.AnomalyDetected = true
			result.AnomalyDetails = fmt.Sprintf("Predicted QoS warning (Overall Pred MOS: %.2f, Sust: %.1f%%)", result.OverallPredictedMOS, result.QoSSustainability)
		}
	}

	return result
}

// ── Notification Worker ────────────────────────────────────────────────────

func (e *AnalyticsEngine) StartNotifier() {
	ticker := time.NewTicker(2 * time.Second)
	go func() {
		for range ticker.C {
			e.mu.RLock()
			subs := make([]*EventSubscription, 0, len(e.subscriptions))
			for _, s := range e.subscriptions {
				subs = append(subs, s)
			}
			e.mu.RUnlock()

			for _, sub := range subs {
				for _, evtType := range sub.EventTypes {
					analytics := e.ComputeAnalytics(evtType)
					if len(analytics.SessionAnalytics) == 0 {
						continue
					}
					notif := EventNotification{
						NotifID:   sub.NotifID,
						EventType: evtType,
						Analytics: *analytics,
					}
					go sendNotification(sub.NotifURI, &notif)
				}
			}
		}
	}()
}

func sendNotification(uri string, notif *EventNotification) {
	body, _ := json.Marshal(notif)
	resp, err := http.Post(uri, "application/json", bytes.NewReader(body))
	if err != nil {
		logWarn("Notif", "Notification to %s failed: %v", uri, err)
		return
	}
	resp.Body.Close()
}

// ── NRF Registration ───────────────────────────────────────────────────────

func registerWithNRF() error {
	profile := map[string]interface{}{
		"nfInstanceId": cfg.NfInstID,
		"nfType":       "NWDAF",
		"nfStatus":     "REGISTERED",
		"ipv4Addresses": []string{cfg.RegisterIP},
		"nfServices": []map[string]interface{}{
			{
				"serviceInstanceId": "nwdaf-events-sub",
				"serviceName":       "nnwdaf-eventssubscription",
				"versions":          []map[string]string{{"apiVersionInUri": "v1", "apiFullVersion": "1.0.0"}},
				"scheme":            "http",
				"nfServiceStatus":   "REGISTERED",
				"ipEndPoints":       []map[string]interface{}{{"ipv4Address": cfg.RegisterIP, "port": 8000}},
			},
			{
				"serviceInstanceId": "nwdaf-analytics-info",
				"serviceName":       "nnwdaf-analyticsinfo",
				"versions":          []map[string]string{{"apiVersionInUri": "v1", "apiFullVersion": "1.0.0"}},
				"scheme":            "http",
				"nfServiceStatus":   "REGISTERED",
				"ipEndPoints":       []map[string]interface{}{{"ipv4Address": cfg.RegisterIP, "port": 8000}},
			},
		},
	}

	body, _ := json.Marshal(profile)
	url := fmt.Sprintf("%s/nnrf-nfm/v1/nf-instances/%s", cfg.NrfUri, cfg.NfInstID)

	req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("NRF registration failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		logInfo("Main", "Registered with NRF successfully (nfInstID: %s)", cfg.NfInstID)
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("NRF registration returned %d: %s", resp.StatusCode, string(respBody))
}

func deregisterFromNRF() {
	url := fmt.Sprintf("%s/nnrf-nfm/v1/nf-instances/%s", cfg.NrfUri, cfg.NfInstID)
	req, _ := http.NewRequest("DELETE", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logError("Main", "Deregister failed: %v", err)
		return
	}
	resp.Body.Close()
	logInfo("Main", "Deregistered from NRF")
}

// ── HTTP Handlers ──────────────────────────────────────────────────────────

var engine = NewAnalyticsEngine()

func handleDataIngestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var batch MetricsBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	engine.IngestMetrics(batch.Metrics)
	w.WriteHeader(204)
}

func handleAnalyticsInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	eventType := r.URL.Query().Get("event-type")
	if eventType == "" {
		eventType = "SERVICE_EXPERIENCE"
	}
	result := engine.ComputeAnalytics(eventType)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func handleEventsSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var sub EventSubscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sub.SubscriptionID = uuid.New().String()

	engine.mu.Lock()
	engine.subscriptions[sub.SubscriptionID] = &sub
	engine.mu.Unlock()

	logInfo("EvtSub", "New subscription: %s → %s (events: %v)", sub.SubscriptionID, sub.NotifURI, sub.EventTypes)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", fmt.Sprintf("/nnwdaf-eventssubscription/v1/subscriptions/%s", sub.SubscriptionID))
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(sub)
}

func handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	// Extract subId from path: /nnwdaf-eventssubscription/v1/subscriptions/{subId}
	subID := r.URL.Path[len("/nnwdaf-eventssubscription/v1/subscriptions/"):]
	engine.mu.Lock()
	delete(engine.subscriptions, subID)
	engine.mu.Unlock()
	logInfo("EvtSub", "Subscription deleted: %s", subID)
	w.WriteHeader(204)
}

func handleSessionRemoval(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID != "" {
		engine.RemoveSession(sessionID)
	}
	w.WriteHeader(204)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "healthy",
		"nfType":     "NWDAF",
		"nfInstID":   cfg.NfInstID,
		"numSessions": len(engine.metrics),
		"numSubs":    len(engine.subscriptions),
	})
}

// ── CORS Middleware ────────────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logInfo("Main", "Starting NWDAF (bind: %s)", cfg.BindAddr)

	// Parse CLI args
	for i, arg := range os.Args {
		if arg == "-c" && i+1 < len(os.Args) {
			logInfo("CFG", "Config file: %s", os.Args[i+1])
		}
	}

	// Register with NRF
	for i := 0; i < 10; i++ {
		if err := registerWithNRF(); err != nil {
			logWarn("Main", "Registration attempt %d failed: %v", i+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}

	// Start notification worker and session cleaner
	engine.StartNotifier()
	engine.StartCleaner()

	// Set up routes
	mux := http.NewServeMux()
	mux.HandleFunc("/nwdaf-dataingestion/v1/metrics", handleDataIngestion)
	mux.HandleFunc("/nwdaf-dataingestion/v1/session", handleSessionRemoval)
	mux.HandleFunc("/nnwdaf-analyticsinfo/v1/analytics", handleAnalyticsInfo)
	mux.HandleFunc("/nnwdaf-eventssubscription/v1/subscriptions", handleEventsSubscription)
	mux.HandleFunc("/nnwdaf-eventssubscription/v1/subscriptions/", handleDeleteSubscription)
	mux.HandleFunc("/nnwdaf-oam/v1/", handleHealth)

	logInfo("SBI", "SBI server listening on %s", cfg.BindAddr)
	if err := http.ListenAndServe(cfg.BindAddr, corsMiddleware(mux)); err != nil {
		logError("SBI", "Server error: %v", err)
	}
}
