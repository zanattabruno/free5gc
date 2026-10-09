package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Rate updates preserve the socket, epoch, packet sequence and byte counters.
type labSender struct {
	mu               sync.Mutex
	conn             *net.UDPConn
	flow             labFlow
	epoch            uint32
	seq              uint64
	bytes            uint64
	errors           uint64
	requestedBuffer  int
	effectiveBuffer  int
	dataErrors       labSocketErrors
	probeWriteErrors labSocketErrors
	probeReadErrors  labSocketErrors
}

// Preserve the original writeErrors counter, with bounded diagnostic detail.
type labSocketError struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Errno   int       `json:"errno"`
	Timeout bool      `json:"timeout"`
	Message string    `json:"message"`
}
type labSocketErrors struct {
	Count    uint64            `json:"count"`
	Timeouts uint64            `json:"timeouts"`
	ByType   map[string]uint64 `json:"byType"`
	First    *labSocketError   `json:"first,omitempty"`
	Last     *labSocketError   `json:"last,omitempty"`
	Events   []labSocketError  `json:"events,omitempty"`
	Omitted  uint64            `json:"omitted"`
}

func (d *labSocketErrors) record(err error) {
	if err == nil {
		return
	}
	e := labSocketError{At: time.Now().UTC(), Type: fmt.Sprintf("%T", err), Message: err.Error()}
	var number syscall.Errno
	if errors.As(err, &number) {
		e.Errno = int(number)
		e.Type = number.Error()
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		e.Timeout = true
		e.Type = "timeout"
		d.Timeouts++
	}
	if d.ByType == nil {
		d.ByType = map[string]uint64{}
	}
	d.Count++
	d.ByType[e.Type]++
	if d.First == nil {
		first := e
		d.First = &first
	}
	d.Last = &e
	if len(d.Events) < 64 {
		d.Events = append(d.Events, e)
	} else {
		d.Omitted++
	}
}

func (s *labSender) emit() {
	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	last := time.Now()
	credit := 0.0
	var seq uint64
	b := make([]byte, labPayload)
	binary.BigEndian.PutUint32(b, labMagic)
	binary.BigEndian.PutUint32(b[4:], s.epoch)
	for now := range t.C {
		s.mu.Lock()
		enabled, rate := s.flow.Enabled, s.flow.Mbps
		s.mu.Unlock()
		dt := now.Sub(last).Seconds()
		last = now
		if !enabled {
			credit = 0
			continue
		}
		credit += dt * rate * 1e6 / (labPayload * 8)
		var count, errors uint64
		_ = s.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		for credit >= 1 {
			binary.BigEndian.PutUint64(b[8:], seq+1)
			binary.BigEndian.PutUint64(b[16:], uint64(time.Now().UnixNano()))
			if _, err := s.conn.Write(b); err != nil {
				s.mu.Lock()
				s.dataErrors.record(err)
				s.mu.Unlock()
				errors++
				credit = 0
				break
			}
			seq++
			count++
			credit--
		}
		s.mu.Lock()
		s.seq = seq
		s.bytes += count * labPayload
		s.errors += errors
		s.mu.Unlock()
	}
}
func (s *labSender) echo() {
	b := make([]byte, 2048)
	for {
		n, e := s.conn.Read(b)
		if e != nil {
			s.mu.Lock()
			s.probeReadErrors.record(e)
			s.mu.Unlock()
			return
		}
		if n == 24 && binary.BigEndian.Uint32(b) == labProbeMagic {
			if _, err := s.conn.Write(b[:n]); err != nil {
				s.mu.Lock()
				s.probeWriteErrors.record(err)
				s.mu.Unlock()
			}
		}
	}
}
func runLabSender() error {
	bufferMiB := 32
	if value := os.Getenv("QOS_LAB_SENDER_BUFFER_MIB"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || (parsed != 8 && parsed != 32) {
			return fmt.Errorf("sender buffer must be 8 or 32 MiB")
		}
		bufferMiB = parsed
	}
	var mu sync.Mutex
	senders := map[int]*labSender{}
	mux := http.NewServeMux()
	mux.HandleFunc("/flows", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var f labFlow
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&f) != nil || net.ParseIP(f.UEIP).To4() == nil || f.ServerPort < 5200 || f.ServerPort > 5298 || f.UEPort < 6000 || f.UEPort > 6098 || f.Mbps < 0 || f.Mbps > 100 {
			http.Error(w, "invalid lab flow", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		s := senders[f.ServerPort]
		if s != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.flow.UEIP != f.UEIP || s.flow.UEPort != f.UEPort {
				http.Error(w, "sender port belongs to another flow", 409)
				return
			}
			s.flow = f
		} else {
			if !f.Enabled {
				w.WriteHeader(200)
				return
			}
			c, e := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: f.ServerPort}, &net.UDPAddr{IP: net.ParseIP(f.UEIP), Port: f.UEPort})
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			// IFB ingress queues retain socket accounting. Reserve enough buffer on
			// this owned socket to offer the configured load instead of blocking
			// the generator at the shaped receiver rate. No host sysctl changes.
			requested, effective := bufferMiB<<20, 0
			raw, err := c.SyscallConn()
			if err == nil {
				err = raw.Control(func(fd uintptr) {
					e = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUFFORCE, requested)
					if e == nil {
						effective, e = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
					}
				})
			}
			if err != nil || e != nil {
				c.Close()
				http.Error(w, "cannot provision lab UDP send buffer", 500)
				return
			}
			s = &labSender{conn: c, flow: f, epoch: uint32(time.Now().UnixNano()), requestedBuffer: requested, effectiveBuffer: effective}
			senders[f.ServerPort] = s
			go s.emit()
			go s.echo()
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := map[int]any{}
		for port, s := range senders {
			s.mu.Lock()
			encoded, _ := json.Marshal(map[string]any{"flow": s.flow, "epoch": s.epoch, "packets": s.seq, "payloadBytes": s.bytes, "ipBytes": s.bytes + s.seq*28, "writeErrors": s.errors,
				"requestedSendBufferBytes": s.requestedBuffer, "effectiveSendBufferBytes": s.effectiveBuffer,
				"dataWriteDiagnostics": s.dataErrors, "probeWriteDiagnostics": s.probeWriteErrors, "probeReadDiagnostics": s.probeReadErrors})
			out[port] = json.RawMessage(encoded)
			s.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"timestamp": time.Now().UTC(), "flows": out})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	return (&http.Server{Addr: ":9093", Handler: mux, ReadHeaderTimeout: 3 * time.Second}).ListenAndServe()
}
