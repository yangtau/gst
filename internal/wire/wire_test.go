package wire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{Method: "daemon.status"}); err != nil {
		t.Fatal(err)
	}
	if n := binary.BigEndian.Uint32(buf.Bytes()); int(n) != buf.Len()-4 {
		t.Fatalf("length prefix %d for %d payload bytes", n, buf.Len()-4)
	}
	// Unknown fields are ignored, so that new fields stay compatible.
	if err := WriteFrame(&buf, map[string]any{"method": "x", "future": 1}); err != nil {
		t.Fatal(err)
	}
	var req Request
	for _, want := range []string{"daemon.status", "x"} {
		if err := ReadFrame(&buf, &req); err != nil || req.Method != want {
			t.Fatalf("read %+v, %v; want method %s", req, err, want)
		}
	}
	if err := ReadFrame(&buf, &req); err != io.EOF {
		t.Fatalf("clean end gave %v", err)
	}

	var big bytes.Buffer
	binary.Write(&big, binary.BigEndian, uint32(MaxFrame+1))
	if err := ReadFrame(&big, &req); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame gave %v", err)
	}
	if err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 5, '{'}), &req); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame gave %v", err)
	}
	if err := WriteFrame(io.Discard, strings.Repeat("x", MaxFrame)); err == nil {
		t.Fatal("oversized frame written")
	}
}

// accept answers the control role and refuses others, as a daemon would.
func accept(conn net.Conn) (Hello, error) {
	theirs, err := ReadHello(conn)
	if err != nil {
		return theirs, err
	}
	var refusal *Error
	if theirs.Role != RoleControl {
		refusal = Errorf(CodeUnsupportedRole, "no")
	}
	return theirs, Answer(conn, theirs, Hello{Software: "gst test", Role: RoleControl}, refusal)
}

func TestHandshakeAndCalls(t *testing.T) {
	var s Server
	Handle(&s, "echo", func(_ context.Context, p struct{ Text string }) (any, error) { return p, nil })
	Handle(&s, "empty", func(context.Context, struct{}) (any, error) { return nil, nil })
	Handle(&s, "fail", func(context.Context, struct{}) (any, error) { return nil, errors.New("it broke") })
	Handle(&s, "missing", func(context.Context, struct{}) (any, error) { return nil, Errorf(CodeNotFound, "gone") })
	// A stream method answers and then owns the stream: here it sends the
	// text back raw.
	HandleStream(&s, "raw", func(_ context.Context, conn net.Conn, p struct{ Text string }, accept func(any) error) error {
		if p.Text == "" {
			return Errorf(CodeInvalidParams, "no text")
		}
		if err := accept(nil); err != nil {
			return err
		}
		_, err := conn.Write([]byte(p.Text))
		return err
	})

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		hello, err := accept(server)
		if err != nil || hello.Software != "cli" {
			done <- err
			return
		}
		done <- s.Serve(context.Background(), server)
	}()

	hello, err := Dial(client, Hello{Software: "cli", Role: RoleControl})
	if err != nil || hello.Software != "gst test" || hello.Protocol != Protocol {
		t.Fatalf("handshake gave %+v, %v", hello, err)
	}
	var out struct{ Text string }
	if err := Call(client, "echo", map[string]string{"Text": "hi"}, &out); err != nil || out.Text != "hi" {
		t.Fatalf("echo gave %+v, %v", out, err)
	}
	if err := Call(client, "empty", nil, nil); err != nil {
		t.Fatal(err)
	}
	for method, code := range map[string]string{"nope": CodeUnsupportedMethod, "fail": CodeFailed, "missing": CodeNotFound, "raw": CodeInvalidParams} {
		var werr *Error
		if err := Call(client, method, nil, nil); !errors.As(err, &werr) || werr.Code != code {
			t.Fatalf("%s gave %v, want %s", method, err, code)
		}
	}
	var werr *Error
	if err := Call(client, "echo", "not an object", &out); !errors.As(err, &werr) || werr.Code != CodeInvalidParams {
		t.Fatalf("bad params gave %v", err)
	}
	if err := Call(client, "raw", map[string]string{"Text": "bytes"}, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(client)
	if string(data) != "bytes" {
		t.Fatalf("stream carried %q", data)
	}
	if err := <-done; err != nil {
		t.Fatalf("server ended with %v", err)
	}
}

func TestHandshakeRefusal(t *testing.T) {
	for _, tt := range []struct {
		hello Hello
		code  string
	}{
		{Hello{Role: RolePeer, Protocol: 1}, CodeUnsupportedRole},
		{Hello{Role: RoleControl, Protocol: 0}, CodeUnsupportedProto},
	} {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			accept(server)
		}()
		// Dial always sends the current protocol, so write the hello directly.
		if err := WriteFrame(client, tt.hello); err != nil {
			t.Fatal(err)
		}
		var reply Hello
		if err := ReadFrame(client, &reply); err != nil || reply.Error == nil || reply.Error.Code != tt.code {
			t.Errorf("%+v: reply %+v, %v; want %s", tt.hello, reply, err, tt.code)
		}
		client.Close()
	}
}
