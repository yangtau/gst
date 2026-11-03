// Package wire implements the gst protocol, which gst commands use to talk
// to the gst daemon and, in later versions, daemons use to talk to each
// other. docs/design/mesh.md describes it.
//
// Messages are frames: a big-endian uint32 length followed by that many
// bytes of JSON. Every connection starts with an exchange of Hello frames.
// After that, a stream carries calls: a Request frame answered by a
// Response frame. Receivers ignore unknown JSON fields, so new fields and
// methods extend the protocol without a new version.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Protocol is the highest protocol version this implementation speaks.
const Protocol = 1

// MaxFrame is the largest frame payload accepted, in bytes.
const MaxFrame = 1 << 20

// Roles of a connection.
const (
	RoleControl = "control" // a gst command talking to the daemon on its machine
	RolePeer    = "peer"    // two daemons linked over SSH
)

// Hello opens a connection. The connecting side sends one; the accepting
// side answers with its own, setting Error when it refuses the connection.
type Hello struct {
	Protocol int      `json:"protocol"`
	Software string   `json:"software"`
	Role     string   `json:"role"`
	Build    string   `json:"build,omitempty"`
	Features []string `json:"features,omitempty"`
	Error    *Error   `json:"error,omitempty"`
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
	CodeFailed            = "failed"
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
// other side's Hello. A refusal is returned as an *Error.
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
	return theirs, nil
}

// Accept performs the accepting side's handshake on rw. check may refuse
// the connection by returning an Error, which is sent to the other side
// and returned.
func Accept(rw io.ReadWriter, mine Hello, check func(Hello) *Error) (Hello, error) {
	var theirs Hello
	if err := ReadFrame(rw, &theirs); err != nil {
		return Hello{}, err
	}
	mine.Protocol = Protocol
	if theirs.Protocol < 1 {
		mine.Error = Errorf(CodeUnsupportedProto, "protocol version %d is not supported", theirs.Protocol)
	} else if check != nil {
		mine.Error = check(theirs)
	}
	if err := WriteFrame(rw, mine); err != nil {
		return theirs, err
	}
	if mine.Error != nil {
		return theirs, mine.Error
	}
	return theirs, nil
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

// Handler answers one request. It returns a value to encode as the result
// or an Error.
type Handler func(method string, params json.RawMessage) (any, *Error)

// Serve answers requests on a stream until it ends or a frame cannot be
// read or written. A clean end returns nil.
func Serve(rw io.ReadWriter, handle Handler) error {
	for {
		var req Request
		if err := ReadFrame(rw, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		result, callErr := handle(req.Method, req.Params)
		resp := Response{Error: callErr}
		if callErr == nil && result != nil {
			raw, err := json.Marshal(result)
			if err != nil {
				resp.Error = Errorf(CodeFailed, "result encoding failed")
			} else {
				resp.Result = raw
			}
		}
		if err := WriteFrame(rw, resp); err != nil {
			return err
		}
	}
}

// DecodeParams decodes a request's params into v; missing params leave v
// unchanged.
func DecodeParams(params json.RawMessage, v any) *Error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return Errorf(CodeInvalidParams, "invalid params")
	}
	return nil
}
