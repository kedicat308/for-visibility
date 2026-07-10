// Package ingest holds the event-driven collectors that feed the cache.
// fpm.go accepts the TCP connection that FRR's zebra dials out (dplane_fpm_nl,
// `fpm address <shell> port 2620`) and turns streamed Netlink messages into gNMI
// updates under /network-instances/.../afts. It tracks RTM_NEWNEXTHOP objects so
// routes referencing a nexthop id (modern kernels) resolve to a real next-hop.
package ingest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"

	"frr-visible/internal/state"
)

// FPM is the Forwarding Plane Manager ingester (zebra -> shell, route/FIB push).
type FPM struct {
	addr string
	c    *state.Cache
	vrf  *VRFResolver

	mu sync.RWMutex
	nh map[uint32]nexthop // nexthop-object id -> resolved nexthop
}

type nexthop struct {
	gw        net.IP
	oif       uint32
	group     []uint32 // member nexthop ids (for groups)
	blackhole bool
}

func NewFPM(addr string, c *state.Cache) *FPM {
	return &FPM{addr: addr, c: c, vrf: NewVRFResolver(), nh: map[uint32]nexthop{}}
}

// FPM framing: [version u8][msg_type u8][length u16 BE], then a Netlink message.
const (
	fpmHdrLen = 4
	fpmTypeNL = 1
	nlHdrLen  = 16 // nlmsghdr
	rtMsgLen  = 12 // rtmsg
	nhMsgLen  = 8  // nhmsg

	rtmNewRoute   = 24
	rtmDelRoute   = 25
	rtmNewNexthop = 104
	rtmDelNexthop = 105

	afInet  = 2
	afInet6 = 10

	// rtattr types (route)
	rtaDST     = 1
	rtaOIF     = 4
	rtaGateway = 5
	rtaTable   = 15
	rtaNHID    = 30

	// nhattr types (nexthop object)
	nhaID        = 1
	nhaGroup     = 2
	nhaBlackhole = 3
	nhaOIF       = 5
	nhaGateway   = 6

	// rtm_type
	rtnBlackhole   = 6
	rtnUnreachable = 7
	rtnProhibit    = 8
)

func (f *FPM) Run() error {
	l, err := net.Listen("tcp", f.addr)
	if err != nil {
		return err
	}
	log.Printf("[fpm] listening on %s", f.addr)
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		log.Printf("[fpm] zebra connected from %s", conn.RemoteAddr())
		go f.handle(conn)
	}
}

func (f *FPM) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 1<<16)
	hdr := make([]byte, fpmHdrLen)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			log.Printf("[fpm] connection closed: %v", err)
			return
		}
		msgLen := int(binary.BigEndian.Uint16(hdr[2:4]))
		if msgLen < fpmHdrLen {
			log.Printf("[fpm] bad frame len %d", msgLen)
			return
		}
		body := make([]byte, msgLen-fpmHdrLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		if hdr[1] != fpmTypeNL {
			continue
		}
		f.handleNetlink(body)
	}
}

func (f *FPM) handleNetlink(b []byte) {
	if len(b) < nlHdrLen {
		return
	}
	switch binary.LittleEndian.Uint16(b[4:6]) {
	case rtmNewNexthop:
		f.handleNexthop(b, false)
	case rtmDelNexthop:
		f.handleNexthop(b, true)
	case rtmNewRoute:
		f.handleRoute(b, false)
	case rtmDelRoute:
		f.handleRoute(b, true)
	}
}

// handleNexthop parses an RTM_NEWNEXTHOP/DELNEXTHOP object and updates the map.
func (f *FPM) handleNexthop(b []byte, del bool) {
	if len(b) < nlHdrLen+nhMsgLen {
		return
	}
	attrs := b[nlHdrLen+nhMsgLen:]
	var id, oif uint32
	var gw net.IP
	var group []uint32
	var bh bool
	forEachAttr(attrs, func(t uint16, p []byte) {
		switch t {
		case nhaID:
			if len(p) >= 4 {
				id = binary.LittleEndian.Uint32(p)
			}
		case nhaOIF:
			if len(p) >= 4 {
				oif = binary.LittleEndian.Uint32(p)
			}
		case nhaGateway:
			gw = net.IP(append([]byte(nil), p...))
		case nhaBlackhole:
			bh = true
		case nhaGroup:
			// array of struct nexthop_grp { u32 id; u8 weight; u8; u16 } = 8 bytes
			for len(p) >= 8 {
				group = append(group, binary.LittleEndian.Uint32(p[0:4]))
				p = p[8:]
			}
		}
	})
	if id == 0 {
		return
	}
	f.mu.Lock()
	if del {
		delete(f.nh, id)
	} else {
		f.nh[id] = nexthop{gw: gw, oif: oif, group: group, blackhole: bh}
	}
	f.mu.Unlock()
}

// resolveNH follows a nexthop id (and one level of groups) to a concrete nexthop.
func (f *FPM) resolveNH(id uint32, depth int) (gw net.IP, oif uint32, blackhole bool) {
	if depth > 4 {
		return nil, 0, false
	}
	f.mu.RLock()
	nh, ok := f.nh[id]
	f.mu.RUnlock()
	if !ok {
		return nil, 0, false
	}
	if nh.blackhole {
		return nil, 0, true
	}
	if nh.gw != nil || nh.oif != 0 {
		return nh.gw, nh.oif, false
	}
	if len(nh.group) > 0 {
		return f.resolveNH(nh.group[0], depth+1)
	}
	return nil, 0, false
}

func (f *FPM) handleRoute(b []byte, del bool) {
	if len(b) < nlHdrLen+rtMsgLen {
		return
	}
	rtm := b[nlHdrLen:]
	family := rtm[0]
	dstLen := rtm[1]
	rtType := rtm[7]
	table := uint32(rtm[4])

	var dst, gw net.IP
	var oif, rtaTbl, nhid uint32
	forEachAttr(rtm[rtMsgLen:], func(t uint16, p []byte) {
		switch t {
		case rtaDST:
			dst = net.IP(append([]byte(nil), p...))
		case rtaGateway:
			gw = net.IP(append([]byte(nil), p...))
		case rtaOIF:
			if len(p) >= 4 {
				oif = binary.LittleEndian.Uint32(p)
			}
		case rtaTable:
			if len(p) >= 4 {
				rtaTbl = binary.LittleEndian.Uint32(p)
			}
		case rtaNHID:
			if len(p) >= 4 {
				nhid = binary.LittleEndian.Uint32(p)
			}
		}
	})
	if rtaTbl != 0 {
		table = rtaTbl
	}

	prefix, afName := formatPrefix(family, dst, dstLen)
	if prefix == "" {
		return
	}

	blackhole := rtType == rtnBlackhole || rtType == rtnUnreachable || rtType == rtnProhibit
	if gw == nil && nhid != 0 {
		g, o, bh := f.resolveNH(nhid, 0)
		gw = g
		if o != 0 {
			oif = o
		}
		blackhole = blackhole || bh
	}
	nhStr := "-"
	switch {
	case blackhole:
		nhStr = "blackhole"
	case gw != nil:
		nhStr = gw.String()
	}

	vrf := f.vrf.Name(table)
	op := "NEW"
	if del {
		op = "DEL"
	}
	log.Printf("[fpm] %s vrf=%s %s nh=%s oif=%d", op, vrf, prefix, nhStr, oif)

	path := aftPath(vrf, afName, prefix)
	if del {
		_ = f.c.Update("openconfig", nil, []*gnmipb.Path{path})
	} else {
		_ = f.c.Update("openconfig", []*gnmipb.Update{{Path: path, Val: strVal(nhStr)}}, nil)
	}
}

// forEachAttr walks a Netlink rtattr/nlattr TLV stream (4-byte aligned).
func forEachAttr(attrs []byte, fn func(atype uint16, payload []byte)) {
	for len(attrs) >= 4 {
		alen := binary.LittleEndian.Uint16(attrs[0:2])
		atype := binary.LittleEndian.Uint16(attrs[2:4])
		if int(alen) < 4 || int(alen) > len(attrs) {
			return
		}
		fn(atype, attrs[4:alen])
		attrs = attrs[(int(alen)+3)&^3:]
	}
}

func formatPrefix(family byte, dst net.IP, dstLen byte) (prefix, afName string) {
	switch family {
	case afInet:
		ip := make(net.IP, net.IPv4len)
		copy(ip, dst.To4())
		return fmt.Sprintf("%s/%d", ip.String(), dstLen), "ipv4-unicast"
	case afInet6:
		ip := make(net.IP, net.IPv6len)
		copy(ip, dst)
		return fmt.Sprintf("%s/%d", ip.String(), dstLen), "ipv6-unicast"
	default:
		return "", ""
	}
}

func strVal(s string) *gnmipb.TypedValue {
	return &gnmipb.TypedValue{Value: &gnmipb.TypedValue_StringVal{StringVal: s}}
}

// aftPath: /network-instances/network-instance[name=vrf]/afts/<af>/<entry>[prefix=p]/state/next-hop
func aftPath(vrf, afName, prefix string) *gnmipb.Path {
	entry := "ipv4-entry"
	if afName == "ipv6-unicast" {
		entry = "ipv6-entry"
	}
	return &gnmipb.Path{Elem: []*gnmipb.PathElem{
		{Name: "network-instances"},
		{Name: "network-instance", Key: map[string]string{"name": vrf}},
		{Name: "afts"},
		{Name: afName},
		{Name: entry, Key: map[string]string{"prefix": prefix}},
		{Name: "state"},
		{Name: "next-hop"},
	}}
}
