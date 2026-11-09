package vnet

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPingGetsItsPacketBack(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go serveEcho(ctx, conn)
	if rtt, err := ping(t.Context(), conn.LocalAddr().String(), 0); err != nil || rtt <= 0 {
		t.Fatalf("ping = %v, %v", rtt, err)
	}
}

func TestPingWithoutAReply(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0") // says nothing
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := ping(ctx, conn.LocalAddr().String(), 0); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ping = %v", err)
	}
}
