package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── VR Traffic Profiles ────────────────────────────────────────────────────

type VRProfile struct {
	Name          string  `json:"name"`
	AvgThroughput float64 `json:"avgThroughputMbps"`
	PeakFactor    float64 `json:"peakFactor"` // I-frame spike multiplier
	BaseLatency   float64 `json:"baseLatencyMs"`
	BaseJitter    float64 `json:"baseJitterMs"`
	FrameRate     float64 `json:"frameRate"`
	IFrameEvery   int     `json:"iFrameEvery"` // I-frame every N frames
}

var Profiles = map[string]VRProfile{
	"360_VIDEO": {
		Name: "360° Video", AvgThroughput: 100, PeakFactor: 2.5,
		BaseLatency: 8, BaseJitter: 1.5, FrameRate: 60, IFrameEvery: 30,
	},
	"INTERACTIVE_VR": {
		Name: "Interactive VR", AvgThroughput: 200, PeakFactor: 3.0,
		BaseLatency: 5, BaseJitter: 1.0, FrameRate: 90, IFrameEvery: 45,
	},
	"CLOUD_GAMING": {
		Name: "Cloud VR Gaming", AvgThroughput: 400, PeakFactor: 3.5,
		BaseLatency: 3, BaseJitter: 0.5, FrameRate: 120, IFrameEvery: 60,
	},
}

// ── External Network Impairment ────────────────────────────────────────────
// Injected by the experiment runner (or dashboard) so that tc/iperf network
// impairment is reflected in the reported VR session metrics and, in turn, in
// the NWDAF MOS. ThroughputFactor is a multiplier in (0, 1]; 1.0 means no
// reduction. The additive fields are added on top of each profile's baseline.

type Impairment struct {
	AddedLatencyMs     float64 `json:"addedLatencyMs"`
	AddedJitterMs      float64 `json:"addedJitterMs"`
	AddedPacketLossPct float64 `json:"addedPacketLossPct"`
	ThroughputFactor   float64 `json:"throughputFactor"`
}

var (
	currentImpairment   = Impairment{ThroughputFactor: 1.0}
	currentImpairmentMu sync.RWMutex
)

func getImpairment() Impairment {
	currentImpairmentMu.RLock()
	defer currentImpairmentMu.RUnlock()
	return currentImpairment
}

func setImpairment(imp Impairment) {
	if imp.ThroughputFactor <= 0 {
		imp.ThroughputFactor = 1.0
	}
	currentImpairmentMu.Lock()
	currentImpairment = imp
	currentImpairmentMu.Unlock()
}

func clearImpairment() {
	currentImpairmentMu.Lock()
	currentImpairment = Impairment{ThroughputFactor: 1.0}
	currentImpairmentMu.Unlock()
}

// ── VR Session ─────────────────────────────────────────────────────────────

type VRSession struct {
	ID         string    `json:"id"`
	ProfileKey string    `json:"profileKey"`
	Profile    VRProfile `json:"profile"`
	UEIP       string    `json:"ueIp"`
	StartedAt  string    `json:"startedAt"`
	Active     bool      `json:"active"`

	mu          sync.RWMutex
	frameCount  int
	currentTP   float64
	currentLat  float64
	currentJit  float64
	currentPL   float64
	history     []MetricSnapshot
	maxHistory  int
}

type MetricSnapshot struct {
	Timestamp  string  `json:"timestamp"`
	Throughput float64 `json:"throughputMbps"`
	Latency    float64 `json:"latencyMs"`
	Jitter     float64 `json:"jitterMs"`
	PacketLoss float64 `json:"packetLossPercent"`
}

func NewSession(profileKey string) *VRSession {
	p, ok := Profiles[profileKey]
	if !ok {
		p = Profiles["INTERACTIVE_VR"]
		profileKey = "INTERACTIVE_VR"
	}
	return &VRSession{
		ID:         fmt.Sprintf("vr-%s", uuid.New().String()[:8]),
		ProfileKey: profileKey,
		Profile:    p,
		UEIP:       fmt.Sprintf("10.60.0.%d", rand.Intn(200)+10),
		StartedAt:  time.Now().Format(time.RFC3339),
		Active:     true,
		maxHistory: 600,
	}
}

func (s *VRSession) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Active {
		return
	}
	s.frameCount++
	p := s.Profile

	// Bursty VR traffic: I-frames are much larger
	isIFrame := s.frameCount%p.IFrameEvery == 0
	baseThroughput := p.AvgThroughput * (0.7 + rand.Float64()*0.3) // P-frame variation
	if isIFrame {
		baseThroughput *= p.PeakFactor * (0.8 + rand.Float64()*0.4)
	}

	// Occasional throughput dip (network congestion)
	if rand.Float64() < 0.02 {
		baseThroughput *= 0.3
	}

	// Latency with occasional spikes
	latency := p.BaseLatency + rand.Float64()*2.0
	if rand.Float64() < 0.05 {
		latency += 10 + rand.Float64()*15 // spike
	}

	// Jitter
	jitter := p.BaseJitter * (0.5 + rand.Float64())
	if rand.Float64() < 0.03 {
		jitter *= 3
	}

	// Packet loss (mostly 0, occasional)
	packetLoss := 0.0
	if rand.Float64() < 0.08 {
		packetLoss = rand.Float64() * 0.5
	}

	// Apply externally injected network impairment (tc/iperf coupling).
	imp := getImpairment()
	if imp.ThroughputFactor > 0 && imp.ThroughputFactor != 1.0 {
		baseThroughput *= imp.ThroughputFactor
	}
	latency += imp.AddedLatencyMs
	jitter += imp.AddedJitterMs
	packetLoss += imp.AddedPacketLossPct

	s.currentTP = math.Round(baseThroughput*100) / 100
	s.currentLat = math.Round(latency*100) / 100
	s.currentJit = math.Round(jitter*100) / 100
	s.currentPL = math.Round(packetLoss*1000) / 1000

	snap := MetricSnapshot{
		Timestamp:  time.Now().Format(time.RFC3339Nano),
		Throughput: s.currentTP,
		Latency:    s.currentLat,
		Jitter:     s.currentJit,
		PacketLoss: s.currentPL,
	}
	s.history = append(s.history, snap)
	if len(s.history) > s.maxHistory {
		s.history = s.history[1:]
	}
}

func (s *VRSession) GetSnapshot() MetricSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return MetricSnapshot{
		Timestamp:  time.Now().Format(time.RFC3339Nano),
		Throughput: s.currentTP,
		Latency:    s.currentLat,
		Jitter:     s.currentJit,
		PacketLoss: s.currentPL,
	}
}

func (s *VRSession) GetHistory(last int) []MetricSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if last <= 0 || last > len(s.history) {
		last = len(s.history)
	}
	start := len(s.history) - last
	result := make([]MetricSnapshot, last)
	copy(result, s.history[start:])
	return result
}

// ── Simulation Engine ──────────────────────────────────────────────────────

type SimEngine struct {
	mu       sync.RWMutex
	sessions map[string]*VRSession
	nwdafURI string
}

func NewSimEngine(nwdafURI string) *SimEngine {
	return &SimEngine{
		sessions: make(map[string]*VRSession),
		nwdafURI: nwdafURI,
	}
}

func (e *SimEngine) AddSession(profileKey string) *VRSession {
	s := NewSession(profileKey)
	e.mu.Lock()
	e.sessions[s.ID] = s
	e.mu.Unlock()
	log.Printf("[SIM] Session started: %s (%s) UE=%s", s.ID, s.Profile.Name, s.UEIP)
	return s
}

func (e *SimEngine) RemoveSession(id string) {
	e.mu.Lock()
	if s, ok := e.sessions[id]; ok {
		s.Active = false
		delete(e.sessions, id)
	}
	e.mu.Unlock()
	// Notify NWDAF
	req, _ := http.NewRequest("DELETE", e.nwdafURI+"/nwdaf-dataingestion/v1/session?sessionId="+id, nil)
	http.DefaultClient.Do(req)
	log.Printf("[SIM] Session stopped: %s", id)
}

func (e *SimEngine) RemoveAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.RemoveSession(id)
	}
}

func (e *SimEngine) Run() {
	// Tick all sessions every 100ms
	ticker := time.NewTicker(100 * time.Millisecond)
	go func() {
		for range ticker.C {
			e.mu.RLock()
			for _, s := range e.sessions {
				s.Tick()
			}
			e.mu.RUnlock()
		}
	}()

	// Push to NWDAF every 500ms
	pushTicker := time.NewTicker(500 * time.Millisecond)
	go func() {
		for range pushTicker.C {
			e.pushToNWDAF()
		}
	}()
}

func (e *SimEngine) pushToNWDAF() {
	e.mu.RLock()
	var metrics []map[string]interface{}
	for _, s := range e.sessions {
		snap := s.GetSnapshot()
		metrics = append(metrics, map[string]interface{}{
			"sessionId":         s.ID,
			"timestamp":         snap.Timestamp,
			"profile":           s.ProfileKey,
			"throughputMbps":    snap.Throughput,
			"latencyMs":         snap.Latency,
			"jitterMs":          snap.Jitter,
			"packetLossPercent": snap.PacketLoss,
			"frameRate":         s.Profile.FrameRate,
		})
	}
	e.mu.RUnlock()

	if len(metrics) == 0 {
		return
	}

	body, _ := json.Marshal(map[string]interface{}{"metrics": metrics})
	resp, err := http.Post(e.nwdafURI+"/nwdaf-dataingestion/v1/metrics", "application/json", bytes.NewReader(body))
	if err != nil {
		return // silently fail, NWDAF may not be ready
	}
	resp.Body.Close()
}

// ── HTTP API ───────────────────────────────────────────────────────────────

var simEngine *SimEngine

type SessionInfo struct {
	ID         string         `json:"id"`
	ProfileKey string         `json:"profileKey"`
	ProfileName string        `json:"profileName"`
	UEIP       string         `json:"ueIp"`
	StartedAt  string         `json:"startedAt"`
	Active     bool           `json:"active"`
	Current    MetricSnapshot `json:"current"`
}

func apiGetSessions(w http.ResponseWriter, r *http.Request) {
	simEngine.mu.RLock()
	defer simEngine.mu.RUnlock()
	var infos []SessionInfo
	for _, s := range simEngine.sessions {
		infos = append(infos, SessionInfo{
			ID: s.ID, ProfileKey: s.ProfileKey, ProfileName: s.Profile.Name,
			UEIP: s.UEIP, StartedAt: s.StartedAt, Active: s.Active,
			Current: s.GetSnapshot(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(infos)
}

func apiGetSessionHistory(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("id")
	simEngine.mu.RLock()
	s, ok := simEngine.sessions[sid]
	simEngine.mu.RUnlock()
	if !ok {
		http.Error(w, "Session not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.GetHistory(120))
}

func apiPostSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Profile string `json:"profile"`
		Count   int    `json:"count"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Profile == "" {
		req.Profile = "INTERACTIVE_VR"
	}
	var created []SessionInfo
	for i := 0; i < req.Count; i++ {
		s := simEngine.AddSession(req.Profile)
		created = append(created, SessionInfo{
			ID: s.ID, ProfileKey: s.ProfileKey, ProfileName: s.Profile.Name,
			UEIP: s.UEIP, StartedAt: s.StartedAt, Active: true,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(created)
}

func apiDeleteSession(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("id")
	if sid == "all" {
		simEngine.RemoveAll()
	} else {
		simEngine.RemoveSession(sid)
	}
	w.WriteHeader(204)
}

func apiGetProfiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Profiles)
}

func apiImpairment(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getImpairment())
	case "POST", "PUT":
		imp := Impairment{ThroughputFactor: 1.0}
		if err := json.NewDecoder(r.Body).Decode(&imp); err != nil {
			http.Error(w, `{"error":"invalid impairment payload"}`, 400)
			return
		}
		setImpairment(imp)
		log.Printf("[VR-SIM] Impairment set: +%.1fms lat, +%.1fms jit, +%.2f%% loss, x%.2f tput",
			imp.AddedLatencyMs, imp.AddedJitterMs, imp.AddedPacketLossPct, imp.ThroughputFactor)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getImpairment())
	case "DELETE":
		clearImpairment()
		log.Printf("[VR-SIM] Impairment cleared")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getImpairment())
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

func apiGetAggregated(w http.ResponseWriter, r *http.Request) {
	simEngine.mu.RLock()
	defer simEngine.mu.RUnlock()
	var totalTP, totalLat, totalJit, totalPL float64
	count := 0
	for _, s := range simEngine.sessions {
		snap := s.GetSnapshot()
		totalTP += snap.Throughput
		totalLat += snap.Latency
		totalJit += snap.Jitter
		totalPL += snap.PacketLoss
		count++
	}
	result := map[string]interface{}{
		"sessionCount":   count,
		"totalThroughput": math.Round(totalTP*100) / 100,
		"avgLatency":     0.0,
		"avgJitter":      0.0,
		"avgPacketLoss":  0.0,
	}
	if count > 0 {
		result["avgLatency"] = math.Round(totalLat/float64(count)*100) / 100
		result["avgJitter"] = math.Round(totalJit/float64(count)*100) / 100
		result["avgPacketLoss"] = math.Round(totalPL/float64(count)*1000) / 1000
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	nwdafURI := "http://127.0.0.15:8000"
	apiAddr := "0.0.0.0:9090"
	dashAddr := "0.0.0.0:3000"
	initSessions := 3

	// Parse CLI
	for i, arg := range os.Args {
		if arg == "-c" && i+1 < len(os.Args) {
			log.Printf("[VR-SIM] Config: %s", os.Args[i+1])
		}
	}

	simEngine = NewSimEngine(nwdafURI)
	simEngine.Run()

	// Create initial sessions
	for i := 0; i < initSessions; i++ {
		profiles := []string{"360_VIDEO", "INTERACTIVE_VR", "CLOUD_GAMING"}
		simEngine.AddSession(profiles[i%len(profiles)])
	}

	// API server
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			apiGetSessions(w, r)
		case "POST":
			apiPostSession(w, r)
		case "DELETE":
			apiDeleteSession(w, r)
		default:
			http.Error(w, "Method not allowed", 405)
		}
	})
	apiMux.HandleFunc("/api/sessions/history", apiGetSessionHistory)
	apiMux.HandleFunc("/api/profiles", apiGetProfiles)
	apiMux.HandleFunc("/api/aggregated", apiGetAggregated)
	apiMux.HandleFunc("/api/impairment", apiImpairment)

	go func() {
		log.Printf("[VR-SIM] API server on %s", apiAddr)
		http.ListenAndServe(apiAddr, cors(apiMux))
	}()

	// Dashboard server (serves static files + proxies API)
	dashMux := http.NewServeMux()

	// Find dashboard dir
	dashDir := "./dashboard"
	if _, err := os.Stat(dashDir); os.IsNotExist(err) {
		dashDir = "./ue-vr-sim/dashboard"
		if _, err := os.Stat(dashDir); os.IsNotExist(err) {
			dashDir = "/home/vmadmin/workspace/free5gc/ue-vr-sim/dashboard"
		}
	}

	dashMux.Handle("/", http.FileServer(http.Dir(dashDir)))

	// Proxy API calls from dashboard
	dashMux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/sessions" || r.URL.Path == "/api/sessions/":
			switch r.Method {
			case "GET":
				apiGetSessions(w, r)
			case "POST":
				apiPostSession(w, r)
			case "DELETE":
				apiDeleteSession(w, r)
			default:
				http.Error(w, "Method not allowed", 405)
			}
		case r.URL.Path == "/api/sessions/history":
			apiGetSessionHistory(w, r)
		case r.URL.Path == "/api/profiles":
			apiGetProfiles(w, r)
		case r.URL.Path == "/api/aggregated":
			apiGetAggregated(w, r)
		case r.URL.Path == "/api/impairment":
			apiImpairment(w, r)
		default:
			http.Error(w, "Not found", 404)
		}
	})

	// NWDAF proxy: forward /nnwdaf-* requests to NWDAF service
	nwdafProxy := func(w http.ResponseWriter, r *http.Request) {
		targetURL := nwdafURI + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
		}
		proxyReq, _ := http.NewRequest(r.Method, targetURL, bytes.NewReader(bodyBytes))
		for k, vv := range r.Header {
			if strings.EqualFold(k, "Host") { continue }
			for _, v := range vv {
				proxyReq.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil {
			http.Error(w, `{"status":"offline","error":"`+err.Error()+`"}`, 502)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
	dashMux.HandleFunc("/nnwdaf-analyticsinfo/", nwdafProxy)
	dashMux.HandleFunc("/nnwdaf-eventssubscription/", nwdafProxy)
	dashMux.HandleFunc("/nnwdaf-oam/", nwdafProxy)

	log.Printf("[VR-SIM] Dashboard on http://localhost:%s", "3000")
	if err := http.ListenAndServe(dashAddr, cors(dashMux)); err != nil {
		log.Fatalf("[VR-SIM] Dashboard server error: %v", err)
	}
}
