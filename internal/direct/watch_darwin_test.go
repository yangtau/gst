package direct

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNetworkMessage(t *testing.T) {
	message := func(kind byte, flags int32) []byte {
		m := make([]byte, 92)
		binary.LittleEndian.PutUint16(m, uint16(len(m)))
		m[2], m[3] = unix.RTM_VERSION, kind
		binary.LittleEndian.PutUint32(m[8:], uint32(flags))
		return m
	}
	for _, c := range []struct {
		name string
		m    []byte
		want bool
	}{
		{"new address", message(unix.RTM_NEWADDR, 0), true},
		{"gone address", message(unix.RTM_DELADDR, 0), true},
		{"interface", message(unix.RTM_IFINFO, 0), true},
		{"default route", message(unix.RTM_ADD, unix.RTF_UP|unix.RTF_GATEWAY|unix.RTF_STATIC), true},
		{"route changed", message(unix.RTM_CHANGE, unix.RTF_UP|unix.RTF_GATEWAY), true},
		{"a neighbor", message(unix.RTM_ADD, unix.RTF_UP|unix.RTF_HOST|unix.RTF_LLINFO), false},
		{"a host by a gateway", message(unix.RTM_DELETE, unix.RTF_UP|unix.RTF_GATEWAY|unix.RTF_HOST|unix.RTF_WASCLONED), false},
		{"a route without a gateway", message(unix.RTM_ADD, unix.RTF_UP), false},
		{"a miss", message(unix.RTM_MISS, 0), false},
		{"too short", []byte{4, 0, 5, unix.RTM_ADD}, false},
	} {
		if got := networkMessage(c.m); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
