//go:build linux && !android

package direct

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNetworkMessages(t *testing.T) {
	message := func(kind uint16, dstLen byte) []byte {
		m := make([]byte, unix.NLMSG_HDRLEN+12) // the header and a struct rtmsg
		binary.NativeEndian.PutUint32(m, uint32(len(m)))
		binary.NativeEndian.PutUint16(m[4:], kind)
		m[unix.NLMSG_HDRLEN+1] = dstLen
		return m
	}
	join := func(ms ...[]byte) []byte {
		var b []byte
		for _, m := range ms {
			b = append(b, m...)
		}
		return b
	}
	for _, c := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"new address", message(unix.RTM_NEWADDR, 0), true},
		{"link", message(unix.RTM_NEWLINK, 0), true},
		{"default route", message(unix.RTM_NEWROUTE, 0), true},
		{"another route", message(unix.RTM_NEWROUTE, 24), false},
		{"a neighbor", message(unix.RTM_NEWNEIGH, 0), false},
		{"one of two", join(message(unix.RTM_DELROUTE, 16), message(unix.RTM_DELADDR, 0)), true},
		{"broken", []byte{200, 0, 0, 0}, false},
	} {
		if got := networkMessages(c.b); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
