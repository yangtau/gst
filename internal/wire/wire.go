// Package wire implements the gst protocol, which gst commands use to talk
// to the gst daemon and daemons use to talk to each other.
// docs/design/mesh.md describes it.
//
// Every connection starts with an exchange of Hello frames: a big-endian
// uint32 length followed by that many bytes of JSON. After that, a control
// connection carries calls, a Request frame answered by a Response frame,
// and a peer connection is a link of the mesh, with frames of its own
// (internal/mesh/link). Receivers ignore unknown JSON fields, so new fields
// and methods extend the protocol without a new version.
package wire

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// Protocol is the protocol version this implementation speaks. Version 3
// made the virtual network the only way to reach machines: it has no
// connections to addresses, and a UDP path carries WireGuard's packets
// rather than QUIC. Version 4 shares one UDP port among all paths, and
// changes the packets on them. Version 5 identifies links by an id
// rather than a name that people choose and address machines by. Version
// 6 splits the mesh into links, a replicated table, and routing: a link is
// a stream of typed frames rather than a yamux session, machines stay
// members while offline, and a removal is a record of the table. It does
// not talk to earlier versions.
const Protocol = 6

// MaxFrame is the largest frame payload accepted, in bytes.
const MaxFrame = 1 << 20

// Roles of a connection.
const (
	RoleControl = "control" // a gst command talking to the daemon on its machine
	RolePeer    = "peer"    // two daemons linked over SSH
)

// Hello opens a connection. The connecting side sends one; the accepting
// side answers with its own, setting Error when it refuses the connection.
// Peers say who they are (Node) and whether they belong to the network
// (Member).
type Hello struct {
	Protocol int       `json:"protocol"`
	Software string    `json:"software"`
	Role     string    `json:"role"`
	Build    string    `json:"build,omitempty"`
	Node     *NodeInfo `json:"node,omitempty"`
	Member   *Member   `json:"member,omitempty"`
	Error    *Error    `json:"error,omitempty"`
}

// Request calls a method.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response answers a Request with either Result or Error.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error codes.
const (
	CodeUnsupportedMethod = "unsupported_method"
	CodeUnsupportedRole   = "unsupported_role"
	CodeUnsupportedProto  = "unsupported_protocol"
	CodeInvalidParams     = "invalid_params"
	CodeNotFound          = "not_found"
	CodeForbidden         = "forbidden"
	CodeFailed            = "failed"
	CodeUnreachable       = "unreachable" // no way to the machine a message is for
	CodeRemoved           = "removed"     // the machine was removed from the network
)

// Error is a failure reported by the other side.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an Error with code and a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WriteFrame writes v as one frame.
func WriteFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds the limit", len(payload))
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	_, err = w.Write(frame)
	return err
}

// ReadFrame reads one frame into v. It returns io.EOF only when the stream
// ends cleanly before a frame starts.
func ReadFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds the limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("invalid frame: %w", err)
	}
	return nil
}

// Dial performs the connecting side's handshake on rw and returns the
// other side's Hello. A refusal, or an answer in an older protocol, is
// returned as an *Error.
func Dial(rw io.ReadWriter, mine Hello) (Hello, error) {
	mine.Protocol = Protocol
	if err := WriteFrame(rw, mine); err != nil {
		return Hello{}, err
	}
	var theirs Hello
	if err := ReadFrame(rw, &theirs); err != nil {
		return Hello{}, err
	}
	if theirs.Error != nil {
		return theirs, theirs.Error
	}
	if theirs.Protocol < Protocol {
		return theirs, olderProtocol(theirs, mine)
	}
	return theirs, nil
}

// olderProtocol is the refusal of theirs, which speaks an older protocol
// than mine. It names both sides' gst, as the other side shows it too.
func olderProtocol(theirs, mine Hello) *Error {
	return Errorf(CodeUnsupportedProto, "%s speaks gst protocol %d and %s speaks %d; upgrade gst on the older side",
		theirs.Software, theirs.Protocol, mine.Software, mine.Protocol)
}

// ReadHello reads the Hello that opens a connection, on the accepting
// side, which answers it with Answer.
func ReadHello(r io.Reader) (Hello, error) {
	var theirs Hello
	err := ReadFrame(r, &theirs)
	return theirs, err
}

// Answer answers theirs with mine, refusing the connection with refusal
// when it is not nil or when theirs speaks an older protocol. A refusal is
// sent to the other side and returned.
func Answer(w io.Writer, theirs, mine Hello, refusal *Error) error {
	mine.Protocol = Protocol
	mine.Error = refusal
	if theirs.Protocol < Protocol {
		mine.Error = olderProtocol(theirs, mine)
	}
	if err := WriteFrame(w, mine); err != nil {
		return err
	}
	if mine.Error != nil {
		return mine.Error
	}
	return nil
}

// Call sends a request on a stream and decodes the result into result,
// which may be nil. A failure reported by the other side is an *Error.
func Call(rw io.ReadWriter, method string, params, result any) error {
	req := Request{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	if err := WriteFrame(rw, req); err != nil {
		return err
	}
	var resp Response
	if err := ReadFrame(rw, &resp); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil || resp.Result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("invalid result of %s: %w", method, err)
	}
	return nil
}

// Server answers the calls on a stream with the methods registered with
// Handle and HandleStream, in order, until the stream ends or a stream
// method takes it over. It answers unknown methods with
// unsupported_method, undecodable params with invalid_params, and a
// failed call with its error: an *Error as it is, any other error with
// code failed and the error's text.
type Server struct {
	methods map[string]method
}

type method struct {
	// call answers one call, with accept or, before accepting, with an
	// error. After accepting, a method that takes over owns the stream,
	// and its error is dropped.
	call      func(ctx context.Context, conn net.Conn, params json.RawMessage, accept func(result any) error) error
	takesOver bool
}

// Handle registers a method that answers with a result, after which the
// stream carries the next call. Missing params leave p zero.
func Handle[P any](s *Server, name string, fn func(ctx context.Context, p P) (any, error)) {
	register(s, name, false, func(ctx context.Context, _ net.Conn, p P, accept func(any) error) error {
		result, err := fn(ctx, p)
		if err != nil {
			return err
		}
		return accept(result)
	})
}

// HandleStream registers a method that takes the stream over: it calls
// accept with its result to answer the call, after which it owns conn for
// whatever the method defines, such as the bytes of a connection, until it
// returns. Returning an error before accepting answers with the error.
func HandleStream[P any](s *Server, name string, fn func(ctx context.Context, conn net.Conn, p P, accept func(result any) error) error) {
	register(s, name, true, fn)
}

func register[P any](s *Server, name string, takesOver bool, fn func(context.Context, net.Conn, P, func(any) error) error) {
	if s.methods == nil {
		s.methods = map[string]method{}
	}
	s.methods[name] = method{takesOver: takesOver, call: func(ctx context.Context, conn net.Conn, params json.RawMessage, accept func(any) error) error {
		var p P
		if len(params) > 0 && json.Unmarshal(params, &p) != nil {
			return Errorf(CodeInvalidParams, "invalid params")
		}
		return fn(ctx, conn, p, accept)
	}}
}

// Serve answers the calls on conn. It returns nil when conn ends cleanly
// between calls or once a stream method that took it over returns.
func (s *Server) Serve(ctx context.Context, conn net.Conn) error {
	for {
		var req Request
		if err := ReadFrame(conn, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		m, ok := s.methods[req.Method]
		if !ok {
			if err := reply(conn, nil, Errorf(CodeUnsupportedMethod, "method %q is not supported", req.Method)); err != nil {
				return err
			}
			continue
		}
		accepted := false
		var replyErr error
		err := m.call(ctx, conn, req.Params, func(result any) error {
			accepted = true
			replyErr = reply(conn, result, nil)
			return replyErr
		})
		switch {
		case !accepted:
			replyErr = reply(conn, nil, AsError(err))
		case m.takesOver:
			return nil
		}
		if replyErr != nil {
			return replyErr
		}
	}
}

// AsError returns err as the *Error to answer a call with.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if err == nil {
		err = errors.New("the call failed")
	}
	return Errorf(CodeFailed, "%v", err)
}

// reply writes the response to a request: result, or callErr when it is
// not nil.
func reply(w io.Writer, result any, callErr *Error) error {
	resp := Response{Error: callErr}
	if callErr == nil && result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			resp.Error = Errorf(CodeFailed, "result encoding failed")
		} else {
			resp.Result = raw
		}
	}
	return WriteFrame(w, resp)
}
