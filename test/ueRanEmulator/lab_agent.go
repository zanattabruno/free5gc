package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/free5gc/aper"
	"github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap"
	"github.com/free5gc/ngap/ngapType"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/sctp"
	"github.com/free5gc/util/mongoapi"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"test"
	"test/ngapTestpacket"
	"time"
)

const labMagic uint32 = 0x514f534c
const labPayload = 1200
const labProbeMagic uint32 = 0x51525454

type labFlow struct {
	ServerIP   string  `json:"serverIp"`
	ServerPort int     `json:"serverPort"`
	UEPort     int     `json:"uePort"`
	Protocol   string  `json:"protocol"`
	Enabled    bool    `json:"enabled"`
	Mbps       float64 `json:"offeredMbps"`
	UEIP       string  `json:"ueIp"`
}
type labMetric struct {
	Timestamp       time.Time `json:"timestamp"`
	Throughput      float64   `json:"throughputMbps"`
	IPThroughput    float64   `json:"ipThroughputMbps"`
	PacketLoss      float64   `json:"packetLossPercent"`
	Jitter          float64   `json:"jitterMs"`
	RTT             *float64  `json:"rttMs,omitempty"`
	Packets         uint64    `json:"packets"`
	QFI             uint8     `json:"qfi"`
	IntervalSeconds float64   `json:"intervalSeconds"`
	PayloadBytes    uint64    `json:"payloadBytes"`
	IPBytes         uint64    `json:"ipBytes"`
	UniquePackets   uint64    `json:"uniquePacketsTotal"`
	HighestSequence uint64    `json:"highestSequence"`
	Duplicates      uint64    `json:"duplicatesTotal"`
	Reordered       uint64    `json:"reorderedTotal"`
	RTTCount        uint64    `json:"rttCount"`
	Epoch           uint64    `json:"epoch"`
	ExpectedPackets uint64    `json:"expectedPackets"`
	LossDelta       int64     `json:"lossDelta"`
}
type receivedFlow struct {
	Flow                                           labFlow     `json:"flow"`
	History                                        []labMetric `json:"history"`
	epoch, lastSeq, received, lost, bytes, ipBytes uint64
	lastTransit                                    float64
	jitter                                         float64
	started                                        time.Time
	qfi                                            uint8
	seen                                           map[uint64]bool
	unique, duplicates, reordered                  uint64
	sampleHighest                                  uint64
	sampleMissing                                  int64
	rttSum                                         float64
	rttCount                                       uint64
	lastRTT                                        *float64
	lastRTTAt                                      time.Time
}
type labAgent struct {
	mu         sync.Mutex
	ue         *test.RanUeContext
	conn       *net.UDPConn
	ip         string
	teid       uint32
	defaultQFI uint8
	flows      map[int]*receivedFlow
	lastRTT    *float64
	lastRTTAt  time.Time
	activeQFIs map[uint8]bool
}

var currentLab *labAgent

func provisionLab(ue *test.RanUeContext) error {
	if e := mongoapi.SetMongoDB("free5gc", "mongodb://127.0.0.1:27017"); e != nil {
		return e
	}
	// These fixtures are written only by the isolated lab invocation.
	test.InsertAuthSubscriptionToMongoDB(ue.Supi, ue.AuthenticationSubs)
	var am models.AccessAndMobilitySubscriptionData
	_ = json.Unmarshal([]byte(`{"gpsis":["msisdn-0900000000"],"subscribedUeAmbr":{"uplink":"1000 Mbps","downlink":"1000 Mbps"},"nssai":{"defaultSingleNssais":[{"sst":1,"sd":"010203"}],"singleNssais":[{"sst":1,"sd":"010203"}]}}`), &am)
	test.InsertAccessAndMobilitySubscriptionDataToMongoDB(ue.Supi, am, "20893")
	var sel models.SmfSelectionSubscriptionData
	_ = json.Unmarshal([]byte(`{"subscribedSnssaiInfos":{"01010203":{"dnnInfos":[{"dnn":"internet"}]}}}`), &sel)
	test.InsertSmfSelectionSubscriptionDataToMongoDB(ue.Supi, sel, "20893")
	var sm []models.SessionManagementSubscriptionData
	_ = json.Unmarshal([]byte(`[{"singleNssai":{"sst":1,"sd":"010203"},"dnnConfigurations":{"internet":{"pduSessionTypes":{"defaultSessionType":"IPV4","allowedSessionTypes":["IPV4"]},"sscModes":{"defaultSscMode":"SSC_MODE_1","allowedSscModes":["SSC_MODE_1"]},"5gQosProfile":{"5qi":9,"arp":{"priorityLevel":8,"preemptCap":"NOT_PREEMPT","preemptVuln":"NOT_PREEMPTABLE"}},"sessionAmbr":{"uplink":"1000 Mbps","downlink":"1000 Mbps"}}}}]`), &sm)
	test.InsertSessionManagementSubscriptionDataToMongoDB(ue.Supi, "20893", sm)
	test.InsertAmPolicyDataToMongoDB(ue.Supi, models.AmPolicyData{})
	var policy models.SmPolicyData
	_ = json.Unmarshal([]byte(`{"smPolicySnssaiData":{"01010203":{"snssai":{"sst":1,"sd":"010203"},"smPolicyDnnData":{"internet":{"dnn":"internet","gbrDl":"100 Mbps","gbrUl":"10 Mbps"}}}}}`), &policy)
	test.InsertSmPolicyDataToMongoDB(ue.Supi, policy)
	return nil
}
func labDecodeNAS(ue *test.RanUeContext, b []byte) (*nas.Message, error) {
	m, e := test.NASDecode(ue, nas.GetSecurityHeaderType(b), b)
	if e != nil {
		return nil, e
	}
	if m.DLNASTransport != nil {
		b = m.DLNASTransport.GetPayloadContainerContents()
		n := nas.NewMessage()
		if e = n.PlainNasDecode(&b); e != nil {
			return nil, e
		}
		return n, nil
	}
	return m, nil
}
func labSetup(ue *test.RanUeContext, conn *net.UDPConn, b []byte) error {
	p, e := ngap.Decoder(b)
	if e != nil {
		return e
	}
	if p.InitiatingMessage == nil || p.InitiatingMessage.Value.PDUSessionResourceSetupRequest == nil {
		return fmt.Errorf("expected PDU setup request")
	}
	a := &labAgent{ue: ue, conn: conn, flows: map[int]*receivedFlow{}, activeQFIs: map[uint8]bool{}}
	for _, x := range p.InitiatingMessage.Value.PDUSessionResourceSetupRequest.ProtocolIEs.List {
		if x.Value.PDUSessionResourceSetupListSUReq == nil {
			continue
		}
		for _, item := range x.Value.PDUSessionResourceSetupListSUReq.List {
			if item.PDUSessionNASPDU != nil {
				n, e := labDecodeNAS(ue, item.PDUSessionNASPDU.Value)
				if e != nil {
					return e
				}
				if n.PDUSessionEstablishmentAccept == nil || n.PDUSessionEstablishmentAccept.PDUAddress == nil {
					return fmt.Errorf("PDU address missing")
				}
				addr := n.PDUSessionEstablishmentAccept.PDUAddress.GetPDUAddressInformation()
				a.ip = net.IP(addr[:4]).String()
			}
			var t ngapType.PDUSessionResourceSetupRequestTransfer
			if e = aper.UnmarshalWithParams(item.PDUSessionResourceSetupRequestTransfer, &t, "valueExt"); e != nil {
				return e
			}
			for _, i := range t.ProtocolIEs.List {
				if v := i.Value.ULNGUUPTNLInformation; v != nil && v.GTPTunnel != nil {
					a.teid = binary.BigEndian.Uint32(v.GTPTunnel.GTPTEID.Value)
				}
				if v := i.Value.QosFlowSetupRequestList; v != nil {
					for _, q := range v.List {
						a.defaultQFI = uint8(q.QosFlowIdentifier.Value)
						a.activeQFIs[a.defaultQFI] = true
					}
				}
			}
		}
	}
	if net.ParseIP(a.ip) == nil || a.teid == 0 || a.defaultQFI == 0 {
		return fmt.Errorf("incomplete PDU binding: IP=%s TEID=%d QFI=%d", a.ip, a.teid, a.defaultQFI)
	}
	currentLab = a
	return nil
}
func (a *labAgent) serve(n2 *sctp.SCTPConn) error {
	go a.receive()
	go a.sample()
	go a.probe()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", a.status)
	mux.HandleFunc("/flows", a.flowControl)
	addr := os.Getenv("QOS_AGENT_BIND")
	if addr == "" {
		addr = "127.0.0.1:9092"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if e := server.ListenAndServe(); e != http.ErrServerClosed {
			fmt.Println("lab API:", e)
		}
	}()
	defer server.Close()
	b := make([]byte, 65535)
	for {
		n, e := n2.Read(b)
		if e != nil {
			return e
		}
		p, e := ngap.Decoder(b[:n])
		if e != nil {
			return e
		}
		if p.InitiatingMessage == nil {
			continue
		}
		if req := p.InitiatingMessage.Value.PDUSessionResourceModifyRequest; req != nil {
			if e = a.modify(n2, req); e != nil {
				return e
			}
		}
	}
}
func (a *labAgent) modify(conn *sctp.SCTPConn, req *ngapType.PDUSessionResourceModifyRequest) error {
	response := ngapTestpacket.BuildPDUSessionResourceModifyResponse(a.ue.AmfUeNgapId, a.ue.RanUeNgapId)
	var result ngapType.PDUSessionResourceModifyListModRes
	for _, x := range req.ProtocolIEs.List {
		if x.Value.PDUSessionResourceModifyListModReq == nil {
			continue
		}
		for _, item := range x.Value.PDUSessionResourceModifyListModReq.List {
			var t ngapType.PDUSessionResourceModifyRequestTransfer
			if e := aper.UnmarshalWithParams(item.PDUSessionResourceModifyRequestTransfer, &t, "valueExt"); e != nil {
				return e
			}
			var out ngapType.PDUSessionResourceModifyResponseTransfer
			for _, i := range t.ProtocolIEs.List {
				if list := i.Value.QosFlowAddOrModifyRequestList; list != nil {
					out.QosFlowAddOrModifyResponseList = &ngapType.QosFlowAddOrModifyResponseList{}
					for _, q := range list.List {
						qfi := uint8(q.QosFlowIdentifier.Value)
						if qfi == 0 || qfi > 63 {
							return fmt.Errorf("invalid QFI")
						}
						a.mu.Lock()
						a.activeQFIs[qfi] = true
						a.mu.Unlock()
						out.QosFlowAddOrModifyResponseList.List = append(out.QosFlowAddOrModifyResponseList.List, ngapType.QosFlowAddOrModifyResponseItem{QosFlowIdentifier: q.QosFlowIdentifier})
					}
				}
				if list := i.Value.QosFlowToReleaseList; list != nil {
					a.mu.Lock()
					for _, q := range list.List {
						delete(a.activeQFIs, uint8(q.QosFlowIdentifier.Value))
					}
					a.mu.Unlock()
				}
			}
			data, e := aper.MarshalWithParams(out, "valueExt")
			if e != nil {
				return e
			}
			result.List = append(result.List, ngapType.PDUSessionResourceModifyItemModRes{PDUSessionID: item.PDUSessionID, PDUSessionResourceModifyResponseTransfer: data})
			if item.NASPDU != nil {
				n, e := labDecodeNAS(a.ue, item.NASPDU.Value)
				if e != nil {
					return e
				}
				if n.PDUSessionModificationCommand == nil {
					return fmt.Errorf("expected NAS modification command")
				}
				// NAS completion is sent only after decoding the actual command.
				m := nas.NewMessage()
				m.GsmMessage = nas.NewGsmMessage()
				m.GsmHeader.SetMessageType(nas.MsgTypePDUSessionModificationComplete)
				m.PDUSessionModificationComplete = nasMessage.NewPDUSessionModificationComplete(0)
				c := m.PDUSessionModificationComplete
				c.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
				c.SetPDUSessionID(uint8(item.PDUSessionID.Value))
				c.SetPTI(n.PDUSessionModificationCommand.GetPTI())
				c.SetMessageType(nas.MsgTypePDUSessionModificationComplete)
				payload, e := m.PlainNasEncode()
				if e != nil {
					return e
				}
				transport := nas.NewMessage()
				transport.GmmMessage = nas.NewGmmMessage()
				transport.GmmHeader.SetMessageType(nas.MsgTypeULNASTransport)
				u := nasMessage.NewULNASTransport(0)
				u.SetSecurityHeaderType(nas.SecurityHeaderTypePlainNas)
				u.SetMessageType(nas.MsgTypeULNASTransport)
				u.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
				u.PduSessionID2Value = nasType.NewPduSessionID2Value(nasMessage.ULNASTransportPduSessionID2ValueType)
				u.SetPduSessionID2Value(uint8(item.PDUSessionID.Value))
				u.SetPayloadContainerType(nasMessage.PayloadContainerTypeN1SMInfo)
				u.PayloadContainer.SetLen(uint16(len(payload)))
				u.PayloadContainer.SetPayloadContainerContents(payload)
				transport.ULNASTransport = u
				ul, e := transport.PlainNasEncode()
				if e != nil {
					return e
				}
				ul, e = test.EncodeNasPduWithSecurity(a.ue, ul, nas.SecurityHeaderTypeIntegrityProtectedAndCiphered, true, false)
				if e != nil {
					return e
				}
				pkt, e := test.GetUplinkNASTransport(a.ue.AmfUeNgapId, a.ue.RanUeNgapId, ul)
				if e != nil {
					return e
				}
				if _, e = conn.Write(pkt); e != nil {
					return e
				}
			}
		}
	}
	resp := response.SuccessfulOutcome.Value.PDUSessionResourceModifyResponse
	filtered := resp.ProtocolIEs.List[:0]
	for _, x := range resp.ProtocolIEs.List {
		if x.Value.PDUSessionResourceModifyListModRes != nil {
			x.Value.PDUSessionResourceModifyListModRes = &result
		}
		if x.Value.PDUSessionResourceFailedToModifyListModRes == nil {
			filtered = append(filtered, x)
		}
	}
	resp.ProtocolIEs.List = filtered
	b, e := ngap.Encoder(response)
	if e != nil {
		return e
	}
	_, e = conn.Write(b)
	return e
}

// parseLabGTP validates lengths and walks extension headers before touching IP.
func parseLabGTP(b []byte) ([]byte, uint8, error) {
	if len(b) < 8 || b[1] != 255 {
		return nil, 0, fmt.Errorf("not GTP-U data")
	}
	end := 8 + int(binary.BigEndian.Uint16(b[2:4]))
	if end > len(b) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	off := 8
	var qfi uint8
	if b[0]&7 != 0 {
		if end < 12 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		next := b[11]
		off = 12
		for next != 0 {
			if off >= end {
				return nil, 0, io.ErrUnexpectedEOF
			}
			n := int(b[off]) * 4
			if n < 4 || off+n > end {
				return nil, 0, io.ErrUnexpectedEOF
			}
			if next == 0x85 {
				qfi = b[off+2] & 63
			}
			next = b[off+n-1]
			off += n
		}
	}
	return b[off:end], qfi, nil
}
func (a *labAgent) receive() {
	b := make([]byte, 65535)
	for {
		n, e := a.conn.Read(b)
		if e != nil {
			return
		}
		if n < 8 || binary.BigEndian.Uint32(b[4:8]) != uerancfg.TeID {
			continue
		}
		packet, qfi, e := parseLabGTP(b[:n])
		if e != nil || len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
			continue
		}
		ihl := int(packet[0]&15) * 4
		if len(packet) < ihl+8 {
			continue
		}
		udp := packet[ihl:]
		port := int(binary.BigEndian.Uint16(udp[2:4]))
		payload := udp[8:]
		if len(payload) < 24 || (binary.BigEndian.Uint32(payload[:4]) != labMagic && binary.BigEndian.Uint32(payload[:4]) != labProbeMagic) {
			continue
		}
		now := time.Now()
		a.mu.Lock()
		if port == 6099 {
			rtt := float64(now.UnixNano()-int64(binary.BigEndian.Uint64(payload[16:24]))) / 1e6
			a.lastRTT = &rtt
			a.lastRTTAt = now
			a.mu.Unlock()
			continue
		}
		f := a.flows[port]
		if f == nil || !f.Flow.Enabled || !a.activeQFIs[qfi] || !net.IP(packet[16:20]).Equal(net.ParseIP(a.ip)) || !net.IP(packet[12:16]).Equal(net.ParseIP(f.Flow.ServerIP)) || int(binary.BigEndian.Uint16(udp[:2])) != f.Flow.ServerPort {
			a.mu.Unlock()
			continue
		}
		if binary.BigEndian.Uint32(payload[:4]) == labProbeMagic {
			rtt := float64(now.UnixNano()-int64(binary.BigEndian.Uint64(payload[16:24]))) / 1e6
			f.lastRTT = &rtt
			f.lastRTTAt = now
			f.rttSum += rtt
			f.rttCount++
			a.mu.Unlock()
			continue
		}
		epoch := uint64(binary.BigEndian.Uint32(payload[4:8]))
		seq := binary.BigEndian.Uint64(payload[8:16])
		if !f.acceptSequence(epoch, seq) {
			a.mu.Unlock()
			continue
		}

		f.received++
		f.bytes += uint64(len(payload))
		f.ipBytes += uint64(len(packet))
		f.qfi = qfi
		transit := float64(now.UnixNano()-int64(binary.BigEndian.Uint64(payload[16:24]))) / 1e6
		if f.lastTransit != 0 {
			f.jitter += (math.Abs(transit-f.lastTransit) - f.jitter) / 16
		}
		f.lastTransit = transit
		a.mu.Unlock()
	}
}
func (a *labAgent) sample() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Now()
	for now := range t.C {
		dt := now.Sub(last).Seconds()
		last = now
		a.mu.Lock()
		for _, f := range a.flows {
			if !f.Flow.Enabled {
				continue
			}
			loss := 0.
			if f.received+f.lost > 0 {
				loss = 100 * float64(f.lost) / float64(f.received+f.lost)
			}
			missing := int64(f.lastSeq) - int64(f.unique)
			expected := f.lastSeq - f.sampleHighest
			lossDelta := missing - f.sampleMissing
			f.sampleHighest = f.lastSeq
			f.sampleMissing = missing
			if expected > 0 {
				loss = math.Max(0, 100*float64(lossDelta)/float64(expected))
			}
			m := labMetric{ExpectedPackets: expected, LossDelta: lossDelta, IntervalSeconds: dt, PayloadBytes: f.bytes, IPBytes: f.ipBytes, UniquePackets: f.unique, HighestSequence: f.lastSeq, Duplicates: f.duplicates, Reordered: f.reordered, RTTCount: f.rttCount, Epoch: f.epoch, Timestamp: now.UTC(), Throughput: float64(f.bytes) * 8 / dt / 1e6, IPThroughput: float64(f.ipBytes) * 8 / dt / 1e6, PacketLoss: loss, Jitter: f.jitter, RTT: f.lastRTT, Packets: f.received, QFI: f.qfi}
			if f.rttCount > 0 {
				avg := f.rttSum / float64(f.rttCount)
				m.RTT = &avg
			}
			if now.Sub(f.lastRTTAt) > 6*time.Second {
				m.RTT = nil
			}
			f.History = append(f.History, m)
			if len(f.History) > 600 {
				f.History = f.History[len(f.History)-600:]
			}
			f.rttSum = 0
			f.rttCount = 0
			f.received = 0
			f.lost = 0
			f.bytes = 0
			f.ipBytes = 0
		}
		a.mu.Unlock()
	}
}

// A probe uses the same IPv4/UDP five-tuple as its application's downlink.
// It is echoed by that sender socket, and is excluded from application bytes.
func (a *labAgent) probe() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		a.mu.Lock()
		for _, f := range a.flows {
			if !f.Flow.Enabled {
				continue
			}
			payload := make([]byte, 24)
			binary.BigEndian.PutUint32(payload, labProbeMagic)
			binary.BigEndian.PutUint64(payload[16:], uint64(time.Now().UnixNano()))
			ip, e := labIPv4UDP(a.ip, f.Flow.ServerIP, uint16(f.Flow.UEPort), uint16(f.Flow.ServerPort), payload)
			if e != nil {
				continue
			}
			qfi := f.qfi
			if !a.activeQFIs[qfi] {
				qfi = a.defaultQFI
			}
			head := []byte{0x34, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x85, 1, 0x10, qfi, 0}
			binary.BigEndian.PutUint16(head[2:], uint16(len(ip)+8))
			binary.BigEndian.PutUint32(head[4:], a.teid)
			_, _ = a.conn.WriteToUDP(append(head, ip...), &net.UDPAddr{IP: net.ParseIP(uerancfg.N3Upf.Addr), Port: int(uerancfg.N3Upf.Port)})
		}
		a.mu.Unlock()
	}
}

// Keep an exact duplicate set for the bounded laboratory run. A sender epoch
// change is explicit evidence of a restart, never silently a rate transition.
func (f *receivedFlow) acceptSequence(epoch, seq uint64) bool {
	if epoch != f.epoch || f.seen == nil {
		f.epoch = epoch
		f.sampleHighest = 0
		f.sampleMissing = 0
		f.lastSeq = 0
		f.unique = 0
		f.seen = map[uint64]bool{}
		f.lastTransit = 0
	}
	if f.seen[seq] {
		f.duplicates++
		return false
	}
	f.seen[seq] = true
	f.unique++
	if seq < f.lastSeq {
		f.reordered++
		if f.lost > 0 {
			f.lost--
		}
	}
	if seq > f.lastSeq {
		f.lost += seq - f.lastSeq - 1
		f.lastSeq = seq
	}
	return true
}
func (a *labAgent) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"device": map[string]interface{}{"ueId": a.ue.Supi, "ueIp": a.ip, "pduSessionId": 10, "dnn": "internet", "snssai": map[string]interface{}{"sst": 1, "sd": "010203"}, "teid": a.teid, "defaultQfi": a.defaultQFI}, "flows": a.flows, "activeQfis": a.activeQFIs})
}
func (a *labAgent) flowControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var f labFlow
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&f) != nil || f.UEPort < 6000 || f.UEPort > 6098 || f.ServerPort < 5200 || f.ServerPort > 5298 || f.ServerIP != "192.0.2.2" || f.Protocol != "UDP" {
		http.Error(w, "invalid lab UDP flow", 400)
		return
	}
	if f.Mbps <= 0 || f.Mbps > 100 {
		f.Mbps = 30
	}
	f.UEIP = a.ip
	b, _ := json.Marshal(f)
	client := http.Client{Timeout: 3 * time.Second}
	resp, e := client.Post("http://192.0.2.2:9093/flows", "application/json", bytes.NewReader(b))
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, "traffic sender rejected request", 502)
		return
	}
	a.mu.Lock()
	old := a.flows[f.UEPort]
	if old == nil {
		old = &receivedFlow{started: time.Now()}
		a.flows[f.UEPort] = old
	}
	old.Flow = f
	a.mu.Unlock()
	w.WriteHeader(200)
}

func labNGSetup() ([]byte, error) {
	p := ngapTestpacket.BuildNGSetupRequest()
	for _, ie := range p.InitiatingMessage.Value.NGSetupRequest.ProtocolIEs.List {
		if v := ie.Value.GlobalRANNodeID; v != nil && v.GlobalGNBID != nil {
			id := uint32(uerancfg.NgapID)
			v.GlobalGNBID.GNBID.GNBID.Bytes = []byte{byte(id >> 16), byte(id >> 8), byte(id)}
			v.GlobalGNBID.GNBID.GNBID.BitLength = 24
		}
		if ie.Value.SupportedTAList == nil {
			continue
		}
		for ti := range ie.Value.SupportedTAList.List {
			ta := &ie.Value.SupportedTAList.List[ti]
			for bi := range ta.BroadcastPLMNList.List {
				for si := range ta.BroadcastPLMNList.List[bi].TAISliceSupportList.List {
					slice := &ta.BroadcastPLMNList.List[bi].TAISliceSupportList.List[si]
					slice.SNSSAI.SST.Value = []byte{1}
					slice.SNSSAI.SD = &ngapType.SD{Value: []byte{1, 2, 3}}
				}
			}
		}
	}
	return ngap.Encoder(p)
}

// Build a complete IPv4 header (including Identification). UDP checksum zero
// is valid for IPv4; the separate echo timestamp measures RTT only.
func labIPv4UDP(src, dst string, sport, dport uint16, payload []byte) ([]byte, error) {
	s, d := net.ParseIP(src).To4(), net.ParseIP(dst).To4()
	if s == nil || d == nil {
		return nil, fmt.Errorf("IPv4 required")
	}
	b := make([]byte, 28+len(payload))
	b[0] = 0x45
	b[8] = 64
	b[9] = 17
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	copy(b[12:16], s)
	copy(b[16:20], d)
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	for sum > 65535 {
		sum = (sum >> 16) + (sum & 65535)
	}
	binary.BigEndian.PutUint16(b[10:], ^uint16(sum))
	binary.BigEndian.PutUint16(b[20:], sport)
	binary.BigEndian.PutUint16(b[22:], dport)
	binary.BigEndian.PutUint16(b[24:], uint16(8+len(payload)))
	copy(b[28:], payload)
	return b, nil
}

func (a *labAgent) setupResponse(encoded []byte) ([]byte, error) {
	p, e := ngap.Decoder(encoded)
	if e != nil {
		return nil, e
	}
	for _, x := range p.SuccessfulOutcome.Value.PDUSessionResourceSetupResponse.ProtocolIEs.List {
		if x.Value.PDUSessionResourceSetupListSURes == nil {
			continue
		}
		for i := range x.Value.PDUSessionResourceSetupListSURes.List {
			item := &x.Value.PDUSessionResourceSetupListSURes.List[i]
			var tr ngapType.PDUSessionResourceSetupResponseTransfer
			if e = aper.UnmarshalWithParams(item.PDUSessionResourceSetupResponseTransfer, &tr, "valueExt"); e != nil {
				return nil, e
			}
			for j := range tr.DLQosFlowPerTNLInformation.AssociatedQosFlowList.List {
				tr.DLQosFlowPerTNLInformation.AssociatedQosFlowList.List[j].QosFlowIdentifier.Value = int64(a.defaultQFI)
			}
			tr.DLQosFlowPerTNLInformation.UPTransportLayerInformation.GTPTunnel.GTPTEID.Value = make([]byte, 4)
			binary.BigEndian.PutUint32(tr.DLQosFlowPerTNLInformation.UPTransportLayerInformation.GTPTunnel.GTPTEID.Value, uerancfg.TeID)
			item.PDUSessionResourceSetupResponseTransfer, e = aper.MarshalWithParams(tr, "valueExt")
			if e != nil {
				return nil, e
			}
		}
	}
	return ngap.Encoder(*p)
}
