package vnet

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsTTL = 10 // seconds; machines change little, but addresses may move

// serveDNS answers queries for names under .gst on conn until ctx is
// done. lookup returns a name's address.
func serveDNS(ctx context.Context, conn net.PacketConn, lookup func(name string) (netip.Addr, bool)) {
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

// answer answers one query, or returns nil for what is no query.
func answer(query []byte, lookup func(string) (netip.Addr, bool)) []byte {
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
	address, found := netip.Addr{}, false
	if machine, ok := strings.CutSuffix(name, ".gst"); ok {
		address, found = lookup(machine)
	}
	switch {
	case !found:
		reply.RCode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET:
		reply.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: dnsTTL},
			Body:   &dnsmessage.AResource{A: address.As4()},
		}}
	}
	packed, err := reply.Pack()
	if err != nil {
		return nil
	}
	return packed
}
