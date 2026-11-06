package vnet

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"net/netip"
	"os"
	"strings"
)

// routes returns the IPv4 routes of this machine that do not go through
// the interface called skip.
func routes(skip string) []netip.Prefix {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []netip.Prefix
	lines := bufio.NewScanner(f)
	lines.Scan() // the header
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 8 || fields[0] == skip {
			continue
		}
		dest, err1 := hex.DecodeString(fields[1])
		mask, err2 := hex.DecodeString(fields[7])
		if err1 != nil || err2 != nil || len(dest) != 4 || len(mask) != 4 {
			continue
		}
		// The kernel prints them in its own byte order, little-endian on
		// every machine gst runs on.
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], binary.BigEndian.Uint32(dest))
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(raw), bits.OnesCount32(binary.BigEndian.Uint32(mask))))
	}
	return out
}
