//go:build integration

// Command gst-it is the network client and server that the integration
// tests run inside their containers, where Go's test process cannot reach.
//
//	gst-it echo-server ADDR                     answer "reply:" + request after the client half-closes
//	gst-it roundtrip [-conns N] [-repeat R] ADDR...
//	                                            send payloads concurrently, connection i to ADDR[i % len], and verify the replies
//	gst-it socks PROXY HOST PORT [PAYLOAD]      connect through a SOCKS5 proxy; print the reply to PAYLOAD, or the first line
//	gst-it line NETWORK ADDR                    print the first line that ADDR sends
//
// It exits with status 1 and a message on stderr when something fails.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
	case "socks":
		err = socks(args)
	case "line":
		err = line(args)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
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
			conn.(*net.TCPConn).CloseWrite()
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

func exchange(conn *net.TCPConn, payload, want string) error {
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

func socks(args []string) error {
	if len(args) != 3 && len(args) != 4 {
		return errors.New("usage: socks PROXY HOST PORT [PAYLOAD]")
	}
	port, err := strconv.ParseUint(args[2], 10, 16)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", args[0], 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	request := append([]byte{5, 1, 0, 5, 1, 0, 3, byte(len(args[1]))}, args[1]...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return err
	}
	reply := make([]byte, 12)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[1] != 0 || reply[3] != 0 {
		return fmt.Errorf("SOCKS reply %v", reply)
	}
	if len(args) == 4 {
		return exchange(conn.(*net.TCPConn), args[3], "")
	}
	return printLine(conn)
}

func line(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: line NETWORK ADDR")
	}
	conn, err := net.DialTimeout(args[0], args[1], 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return printLine(conn)
}

func printLine(r io.Reader) error {
	text, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && text == "" {
		return err
	}
	fmt.Println(strings.TrimSuffix(text, "\n"))
	return nil
}
