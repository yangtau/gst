//go:build integration

// Command gst-it is the network client and server that the integration
// tests run inside their containers, where Go's test process cannot reach.
//
//	gst-it echo-server ADDR                     answer "reply:" + request after the client half-closes
//	gst-it roundtrip [-conns N] [-repeat R] ADDR...
//	                                            send payloads concurrently, connection i to ADDR[i % len], and verify the replies
//	gst-it http METHOD URL [TOKEN]              print the HTTP status and body of a request, with TOKEN as a bearer token;
//	gst-it udp-echo ADDR                        answer every UDP datagram with "reply:" + datagram
//	gst-it udp ADDR PAYLOAD                     send a datagram until a reply comes, within 5 s, and print the reply
//	gst-it dns SERVER NAME                      ask the DNS server at SERVER for NAME's IPv4 addresses and print them
//	gst-it sink ADDR                            accept TCP connections and discard what they send
//	gst-it blast ADDR SECONDS                   send to ADDR over TCP for SECONDS and print the rate in Mbit/s
//	gst-it udp-count ADDR                       count UDP datagrams, and answer "?" with the count, which starts over
//	gst-it udp-burst ADDR MBITS SECONDS         send to a udp-count server at MBITS Mbit/s, a burst for each of 60 frames
//	                                            a second, as video does, and print the share that arrived
//
// It exits with status 1 and a message on stderr when something fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("missing command"))
	}
	var err error
	switch args := os.Args[2:]; os.Args[1] {
	case "echo-server":
		err = echoServer(args)
	case "roundtrip":
		err = roundtrip(args)
	case "http":
		err = httpRequest(args)
	case "udp-echo":
		err = udpEcho(args)
	case "udp":
		err = udpExchange(args)
	case "dns":
		err = dnsLookup(args)
	case "sink":
		err = sink(args)
	case "blast":
		err = blast(args)
	case "udp-count":
		err = udpCount(args)
	case "udp-burst":
		err = udpBurst(args)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func udpEcho(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: udp-echo ADDR")
	}
	conn, err := net.ListenPacket("udp", args[0])
	if err != nil {
		return err
	}
	buf := make([]byte, 65536)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		conn.WriteTo(append([]byte("reply:"), buf[:n]...), from)
	}
}

func udpExchange(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: udp ADDR PAYLOAD")
	}
	conn, err := net.Dial("udp", args[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	buf := make([]byte, 65536)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := conn.Write([]byte(args[1])); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err := conn.Read(buf); err == nil {
			fmt.Println(string(buf[:n]))
			return nil
		}
	}
	return errors.New("no reply")
}

func dnsLookup(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dns SERVER NAME")
	}
	r := net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", args[0])
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := r.LookupIP(ctx, "ip4", args[1])
	if err != nil {
		return err
	}
	for _, a := range addrs {
		fmt.Println(a)
	}
	return nil
}

func sink(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sink ADDR")
	}
	network, address := "tcp", args[0]
	if strings.HasPrefix(address, "unix://") {
		network, address = "unix", strings.TrimPrefix(address, "unix://")
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			io.Copy(io.Discard, conn)
		}()
	}
}

func blast(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: blast ADDR SECONDS")
	}
	seconds, err := strconv.Atoi(args[1])
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", args[0], 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	start := time.Now()
	end := start.Add(time.Duration(seconds) * time.Second)
	conn.SetWriteDeadline(end)
	buf := make([]byte, 64<<10)
	total := 0
	for {
		n, err := conn.Write(buf)
		total += n
		if err != nil {
			break
		}
	}
	fmt.Printf("%.1f\n", float64(total)*8/1e6/time.Since(start).Seconds())
	return nil
}

func udpCount(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: udp-count ADDR")
	}
	conn, err := net.ListenPacket("udp", args[0])
	if err != nil {
		return err
	}
	// What this counts is the network, not how fast it gets to run.
	conn.(*net.UDPConn).SetReadBuffer(4 << 20)
	buf := make([]byte, 65536)
	count := 0
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		if n == 1 && buf[0] == '?' {
			conn.WriteTo([]byte(strconv.Itoa(count)), from)
			count = 0
			continue
		}
		count++
	}
}

func udpBurst(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: udp-burst ADDR MBITS SECONDS")
	}
	mbits, err1 := strconv.Atoi(args[1])
	seconds, err2 := strconv.Atoi(args[2])
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	conn, err := net.Dial("udp", args[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	const size, fps = 1100, 60 // a datagram fits one packet of the network
	packet := make([]byte, size)
	perFrame := mbits * 1000000 / 8 / size / fps
	frame := time.NewTicker(time.Second / fps)
	defer frame.Stop()
	sent := 0
	for range seconds * fps {
		<-frame.C
		for range perFrame {
			if _, err := conn.Write(packet); err == nil {
				sent++
			}
		}
	}
	time.Sleep(500 * time.Millisecond)
	conn.Write([]byte("?"))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	received, err := strconv.Atoi(string(buf[:n]))
	if err != nil {
		return err
	}
	fmt.Printf("%.3f\n", float64(received)/float64(sent))
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gst-it:", err)
	os.Exit(1)
}

func echoServer(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: echo-server ADDR")
	}
	listener, err := net.Listen("tcp", args[0])
	if err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			request, err := io.ReadAll(conn)
			if err != nil {
				return
			}
			conn.Write(append([]byte("reply:"), request...))
			conn.(interface{ CloseWrite() error }).CloseWrite()
		}()
	}
}

func roundtrip(args []string) error {
	fs := flag.NewFlagSet("roundtrip", flag.ContinueOnError)
	conns := fs.Int("conns", 1, "number of concurrent connections")
	repeat := fs.Int("repeat", 1, "times to repeat each connection's payload unit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addrs := fs.Args()
	if len(addrs) == 0 {
		return errors.New("usage: roundtrip [-conns N] [-repeat R] ADDR...")
	}
	var wg sync.WaitGroup
	errs := make([]error, *conns)
	for i := range *conns {
		wg.Go(func() {
			payload := strings.Repeat(fmt.Sprintf("payload-%d/", i), *repeat)
			if err := echoRoundTrip(addrs[i%len(addrs)], payload); err != nil {
				errs[i] = fmt.Errorf("connection %d: %w", i, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// echoRoundTrip sends payload to an echo server through addr and checks the reply.
func echoRoundTrip(addr, payload string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	return exchange(conn.(*net.TCPConn), payload, "reply:"+payload)
}

func exchange(conn interface {
	io.ReadWriter
	CloseWrite() error
}, payload, want string) error {
	if _, err := io.WriteString(conn, payload); err != nil {
		return err
	}
	if err := conn.CloseWrite(); err != nil {
		return err
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if want != "" && string(reply) != want {
		return fmt.Errorf("got %d reply bytes, want %d", len(reply), len(want))
	}
	if want == "" {
		fmt.Print(string(reply))
	}
	return nil
}

func httpRequest(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("usage: http METHOD URL [TOKEN]")
	}
	req, err := http.NewRequest(args[0], args[1], nil)
	if err != nil {
		return err
	}
	if len(args) == 3 {
		req.Header.Set("Authorization", "Bearer "+args[2])
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	fmt.Printf("%d\n%s", res.StatusCode, body)
	return nil
}
