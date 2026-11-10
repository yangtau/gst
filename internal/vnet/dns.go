package vnet

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	dnsTTL      = 10 // seconds; nodes change little, but addresses may move
	dnsNegative = 5  // seconds that resolvers keep the news that a name has no address
)

// dnsZone is the zone that gst answers for.
var dnsZone = dnsmessage.MustNewName("gst.")

// serveDNS answers queries for names under .gst on conn until ctx is
// done. lookup returns a name's address, whether it has one, and whether
// the network runs.
func serveDNS(ctx context.Context, conn net.PacketConn, lookup func(name string) (netip.Addr, bool, bool)) {
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if reply := answer(buf[:n], lookup); reply != nil {
			conn.WriteTo(reply, from)
		}
	}
}

// answer answers one query, or returns nil for what is no query. While
// the network is down it fails, so that resolvers do not keep a missing
// name; a name without an address carries the zone's SOA, whose minimum
// says how long resolvers keep that news.
func answer(query []byte, lookup func(string) (netip.Addr, bool, bool)) []byte {
	var p dnsmessage.Parser
	header, err := p.Start(query)
	if err != nil || header.Response {
		return nil
	}
	questions, err := p.AllQuestions()
	if err != nil || len(questions) != 1 {
		return nil
	}
	q := questions[0]
	reply := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RecursionDesired: header.RecursionDesired},
		Questions: questions,
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	address, found, up := netip.Addr{}, false, true
	if name, ok := strings.CutSuffix(name, ".gst"); ok {
		address, found, up = lookup(name)
	}
	switch {
	case !up:
		reply.RCode = dnsmessage.RCodeServerFailure
	case !found:
		reply.RCode = dnsmessage.RCodeNameError
		reply.Authorities = []dnsmessage.Resource{soa()}
	case q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET:
		reply.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: dnsTTL},
			Body:   &dnsmessage.AResource{A: address.As4()},
		}}
	default:
		reply.Authorities = []dnsmessage.Resource{soa()}
	}
	packed, err := reply.Pack()
	if err != nil {
		return nil
	}
	return packed
}

// soa is the SOA record of the zone, for answers without an address.
func soa() dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsZone, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: dnsNegative},
		Body: &dnsmessage.SOAResource{
			NS: dnsZone, MBox: dnsZone, Serial: 1,
			Refresh: dnsNegative, Retry: dnsNegative, Expire: dnsNegative, MinTTL: dnsNegative,
		},
	}
}
