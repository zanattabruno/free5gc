package main

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestLabUDPIPv4AndGTPFraming(t *testing.T) {
	payload := make([]byte, 24)
	binary.BigEndian.PutUint32(payload, labMagic)
	ip, e := labIPv4UDP("10.60.0.1", "192.0.2.2", 6099, 5299, payload)
	if e != nil {
		t.Fatal(e)
	}
	if len(ip) != 52 || binary.BigEndian.Uint16(ip[2:4]) != 52 || !net.IP(ip[12:16]).Equal(net.ParseIP("10.60.0.1")) || binary.BigEndian.Uint16(ip[20:22]) != 6099 {
		t.Fatal("malformed IPv4/UDP framing")
	}
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	for sum > 65535 {
		sum = (sum >> 16) + (sum & 65535)
	}
	if sum != 65535 {
		t.Fatal("bad IPv4 checksum")
	}
	g := append([]byte{0x34, 0xff, 0, 60, 0, 0, 0, 1, 0, 0, 0, 0x85, 1, 0, 7, 0}, ip...)
	decoded, qfi, e := parseLabGTP(g)
	if e != nil || qfi != 7 || len(decoded) != 52 {
		t.Fatal("wrong network QFI", qfi, e)
	}
	for _, b := range [][]byte{g[:7], g[:16], append([]byte(nil), g[:len(g)-1]...)} {
		if _, _, e = parseLabGTP(b); e == nil {
			t.Fatal("accepted truncated GTP")
		}
	}
}
