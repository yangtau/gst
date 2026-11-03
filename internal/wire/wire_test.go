package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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

func TestHandshakeAndCalls(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		hello, err := Accept(server, Hello{Software: "gst test", Role: RoleControl}, func(h Hello) *Error {
			if h.Role != RoleControl {
				return Errorf(CodeUnsupportedRole, "no")
			}
			return nil
		})
		if err != nil || hello.Software != "cli" {
			done <- err
			return
		}
		done <- Serve(server, func(method string, params json.RawMessage) (any, *Error) {
			switch method {
			case "echo":
				var p struct{ Text string }
				if err := DecodeParams(params, &p); err != nil {
					return nil, err
				}
				return p, nil
			case "empty":
				return nil, nil
			}
			return nil, Errorf(CodeUnsupportedMethod, "method %q is not supported", method)
		})
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
	var werr *Error
	if err := Call(client, "nope", nil, nil); !errors.As(err, &werr) || werr.Code != CodeUnsupportedMethod {
		t.Fatalf("unknown method gave %v", err)
	}
	if err := Call(client, "echo", "not an object", &out); !errors.As(err, &werr) || werr.Code != CodeInvalidParams {
		t.Fatalf("bad params gave %v", err)
	}
	client.Close()
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
			Accept(server, Hello{Role: RoleControl}, func(h Hello) *Error {
				if h.Role != RoleControl {
					return Errorf(CodeUnsupportedRole, "no")
				}
				return nil
			})
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
