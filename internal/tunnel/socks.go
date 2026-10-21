package tunnel

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"

	"golang.org/x/crypto/ssh"
)

// The server side of SOCKS5 (RFC 1928), limited to CONNECT without
// authentication, which is what "ssh -D" offers over direct-tcpip channels.

const (
	socksVersion   = 5
	socksNoAuth    = 0
	socksNoMethods = 0xff
	socksConnect   = 1

	socksIPv4   = 1
	socksDomain = 3
	socksIPv6   = 4
)

// Reply codes.
const (
	socksSucceeded           = 0
	socksGeneralFailure      = 1
	socksNotAllowed          = 2
	socksConnectionRefused   = 5
	socksCommandNotSupported = 7
	socksAddressNotSupported = 8
)

var errSOCKSRequest = errors.New("invalid SOCKS request")

// socksRequest reads the client greeting and CONNECT request from c and
// returns the requested host:port. Domain names are returned unresolved so
// that the server resolves them. Unsupported requests get an error reply.
func socksRequest(c net.Conn) (string, error) {
	var greeting [2]byte // VER NMETHODS
	if _, err := io.ReadFull(c, greeting[:]); err != nil || greeting[0] != socksVersion {
		return "", errSOCKSRequest
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", errSOCKSRequest
	}
	if !slices.Contains(methods, socksNoAuth) {
		c.Write([]byte{socksVersion, socksNoMethods})
		return "", errSOCKSRequest
	}
	if _, err := c.Write([]byte{socksVersion, socksNoAuth}); err != nil {
		return "", err
	}

	var request [4]byte // VER CMD RSV ATYP
	if _, err := io.ReadFull(c, request[:]); err != nil || request[0] != socksVersion {
		return "", errSOCKSRequest
	}
	var host []byte
	switch request[3] {
	case socksIPv4:
		host = make([]byte, 4)
	case socksIPv6:
		host = make([]byte, 16)
	case socksDomain:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", errSOCKSRequest
		}
		host = make([]byte, n[0])
	default:
		socksReply(c, socksAddressNotSupported)
		return "", errSOCKSRequest
	}
	var port [2]byte
	if _, err := io.ReadFull(c, host); err != nil {
		return "", errSOCKSRequest
	}
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", errSOCKSRequest
	}
	if request[1] != socksConnect {
		socksReply(c, socksCommandNotSupported)
		return "", errSOCKSRequest
	}
	name := string(host)
	if request[3] != socksDomain {
		addr, _ := netip.AddrFromSlice(host)
		name = addr.String()
	}
	return net.JoinHostPort(name, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// socksReply answers a request. The bound address is left zero, as
// direct-tcpip channels do not report one.
func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{socksVersion, code, 0, socksIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// socksReplyCode maps the result of dialing a requested address to a reply code.
func socksReplyCode(err error) byte {
	var openErr *ssh.OpenChannelError
	switch {
	case err == nil:
		return socksSucceeded
	case errors.As(err, &openErr) && openErr.Reason == ssh.Prohibited:
		return socksNotAllowed
	case errors.As(err, &openErr) && openErr.Reason == ssh.ConnectionFailed:
		return socksConnectionRefused
	}
	return socksGeneralFailure
}
