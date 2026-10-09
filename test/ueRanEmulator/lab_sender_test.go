package main

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestSocketDiagnosticsPreserveCountsAndBoundDetails(t *testing.T) {
	var diagnostics labSocketErrors
	diagnostics.record(nil)
	for i := 0; i < 70; i++ {
		diagnostics.record(&net.OpError{Op: "write", Net: "udp", Err: os.ErrDeadlineExceeded})
	}
	diagnostics.record(&net.OpError{Op: "write", Net: "udp", Err: &os.SyscallError{Syscall: "write", Err: syscall.ENOBUFS}})
	if diagnostics.Count != 71 || diagnostics.Timeouts != 70 || len(diagnostics.Events) != 64 || diagnostics.Omitted != 7 {
		t.Fatalf("incomplete diagnostics: %+v", diagnostics)
	}
	if diagnostics.First == nil || !diagnostics.First.Timeout || diagnostics.Last == nil || diagnostics.Last.Errno != int(syscall.ENOBUFS) || diagnostics.Last.Timeout {
		t.Fatalf("lost error classification: %+v", diagnostics)
	}
	if diagnostics.ByType["timeout"] != 70 || diagnostics.ByType[syscall.ENOBUFS.Error()] != 1 {
		t.Fatal("error types do not reconcile")
	}
	if diagnostics.Last.At.Before(diagnostics.First.At) {
		t.Fatal("error timestamps out of order")
	}
}
