// bmp.go accepts the TCP connection that FRR's bgpd dials out (BMP, RFC 7854,
// `bmp targets / bmp connect <shell> port 5000`) and turns it into gNMI updates:
//   - Peer Up / Peer Down -> neighbor session-state (openconfig origin)
//   - Route Monitoring (MP_REACH/UNREACH VPN-IPv4) -> L3VPN routes with
//     RD / label / route-target / next-hop / peer (frr origin, control-plane view)
//
// This is the control-plane counterpart to fpm.go's forwarding-plane view.
package ingest

import (
	"bufio"
	"encoding/binary"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"

	"frr-visible/internal/correlate"
	"frr-visible/internal/state"
)

type BMP struct {
	addr string
	c    *state.Cache
	cor  *correlate.Correlator

	vrf   *VRFResolver      // resolves an RD to a VRF name when the wire omits it
	mu    sync.Mutex        // guards rdVRF (default + per-VRF targets dial in concurrently)
	rdVRF map[string]string // Route-Distinguisher -> VRF name, learned once per RD
}

func NewBMP(addr string, c *state.Cache) *BMP {
	return &BMP{addr: addr, c: c, vrf: NewVRFResolver(), rdVRF: map[string]string{}}
}

// SetCorrelator wires the convergence-trace correlator (optional).
func (b *BMP) SetCorrelator(cor *correlate.Correlator) { b.cor = cor }

const (
	bmpVersion    = 3
	bmpHdrLen     = 6
	bmpPerPeerLen = 42

	bmpRouteMon = 0
	bmpStats    = 1
	bmpPeerDown = 2
	bmpPeerUp   = 3
	bmpInit     = 4
	bmpTerm     = 5

	bgpUpdate     = 2
	bgpHdrLen     = 19
	attrMPReach   = 14
	attrMPUnreach = 15
	attrExtComm   = 16

	afiIPv4     = 1
	safiMPLSVPN = 128
)

func (b *BMP) Run() error {
	l, err := net.Listen("tcp", b.addr)
	if err != nil {
		return err
	}
	log.Printf("[bmp] listening on %s", b.addr)
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		log.Printf("[bmp] bgpd connected from %s", conn.RemoteAddr())
		go b.handle(conn)
	}
}

func (b *BMP) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 1<<16)
	hdr := make([]byte, bmpHdrLen)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			log.Printf("[bmp] connection closed: %v", err)
			return
		}
		if hdr[0] != bmpVersion {
			log.Printf("[bmp] bad version %d", hdr[0])
			return
		}
		msgLen := int(binary.BigEndian.Uint32(hdr[1:5]))
		if msgLen < bmpHdrLen {
			return
		}
		body := make([]byte, msgLen-bmpHdrLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		switch hdr[5] {
		case bmpPeerUp:
			b.peerState(body, true)
		case bmpPeerDown:
			b.peerState(body, false)
		case bmpRouteMon:
			b.routeMon(body)
		}
	}
}

// perPeer parses the 42-byte Per-Peer Header (RFC 7854 §4.2).
//
//	off 0     Peer Type (0 global, 1 RD-instance, 2 local-instance, 3 loc-rib)
//	off 1     Peer Flags (bit0x80 = V: peer address is IPv6)
//	off 2..9  Peer Distinguisher (RD when the peer lives in a non-default VRF)
//	off 10..25 Peer Address, off 26..29 Peer AS
func perPeer(body []byte) (peer net.IP, peerAS uint32, ptype byte, rd string, ok bool) {
	if len(body) < bmpPerPeerLen {
		return nil, 0, 0, "", false
	}
	ptype = body[0]
	flags := body[1]
	if !allZero(body[2:10]) {
		rd = formatRD(body[2:10])
	}
	if flags&0x80 != 0 { // V flag set -> IPv6
		peer = net.IP(append([]byte(nil), body[10:26]...))
	} else {
		peer = net.IP(append([]byte(nil), body[22:26]...))
	}
	peerAS = binary.BigEndian.Uint32(body[26:30])
	return peer, peerAS, ptype, rd, true
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func (b *BMP) peerState(body []byte, up bool) {
	peer, peerAS, _, rd, ok := perPeer(body)
	if !ok || peer.IsUnspecified() {
		// Skip the Loc-RIB / unspecified peer (RFC 9069): it has no real neighbor
		// address and would otherwise surface as a bogus 0.0.0.0 BGP neighbor.
		return
	}
	// A non-empty Distinguisher (RD) means the peer lives in a non-default VRF
	// (FRR sets Peer Type=1 / RD-instance for PE-CE eBGP in `router bgp .. vrf X`).
	// Resolve the RD to a VRF name so the neighbor is filed under the same
	// network-instance the FPM AFT uses (e.g. "cust"), learning it once per RD:
	//   1. the Peer Up VRF/Table Name TLV (RFC 9069) when FRR sends it (authoritative);
	//   2. else the node's sole non-default VRF device (a PE usually has one per RD);
	//   3. else fall back to the RD string as the instance name.
	vrf := "default"
	if rd != "" {
		b.mu.Lock()
		name, ok := b.rdVRF[rd]
		b.mu.Unlock()
		if !ok {
			if up {
				name = peerUpVRF(body)
			}
			if name == "" {
				if names := b.vrf.NonDefaultNames(); len(names) == 1 {
					name = names[0]
				}
			}
			if name != "" {
				b.mu.Lock()
				b.rdVRF[rd] = name
				b.mu.Unlock()
			} else {
				name = rd
			}
		}
		vrf = name
	}
	st := "ESTABLISHED"
	if !up {
		st = "IDLE"
	}
	ups := []*gnmipb.Update{leafUpdate(neighborElems(vrf, peer.String(), "session-state"), st)}
	if up && peerAS != 0 {
		ups = append(ups, leafUpdate(neighborElems(vrf, peer.String(), "peer-as"), strconv.FormatUint(uint64(peerAS), 10)))
	}
	_ = b.c.Update("openconfig", ups, nil)
	log.Printf("[bmp] peer %s %s vrf=%s (as=%d)", peer, st, vrf, peerAS)
}

// peerUpVRF extracts the VRF/Table Name (RFC 9069 Information TLV type 3) from a
// BMP Peer Up message body (which starts with the 42-byte per-peer header).
// Layout after the header: Local Address(16) + Local Port(2) + Remote Port(2) +
// Sent OPEN + Received OPEN + Information TLVs. Returns "" when absent.
func peerUpVRF(body []byte) string {
	if len(body) < bmpPerPeerLen {
		return ""
	}
	p := body[bmpPerPeerLen:]
	if len(p) < 20 {
		return ""
	}
	p = p[20:]               // skip local address + local/remote port
	for i := 0; i < 2; i++ { // skip Sent OPEN, then Received OPEN
		if len(p) < bgpHdrLen {
			return ""
		}
		l := int(binary.BigEndian.Uint16(p[16:18]))
		if l < bgpHdrLen || l > len(p) {
			return ""
		}
		p = p[l:]
	}
	for len(p) >= 4 { // Information TLVs: type(2) len(2) value
		t := binary.BigEndian.Uint16(p[0:2])
		l := int(binary.BigEndian.Uint16(p[2:4]))
		if 4+l > len(p) {
			break
		}
		if t == 3 { // VRF/Table Name
			return string(p[4 : 4+l])
		}
		p = p[4+l:]
	}
	return ""
}

func (b *BMP) routeMon(body []byte) {
	peer, _, _, _, ok := perPeer(body)
	if !ok {
		return
	}
	b.parseUpdate(peer, body[bmpPerPeerLen:])
}

// parseUpdate walks a BGP UPDATE for MP_REACH/UNREACH VPN-IPv4 + route-targets.
func (b *BMP) parseUpdate(peer net.IP, m []byte) {
	if len(m) < bgpHdrLen || m[18] != bgpUpdate {
		return
	}
	p := m[bgpHdrLen:]
	if len(p) < 2 {
		return
	}
	wLen := int(binary.BigEndian.Uint16(p[0:2]))
	p = p[2:]
	if len(p) < wLen+2 {
		return
	}
	p = p[wLen:] // skip IPv4-unicast withdrawn (VPN comes via MP attrs)
	paLen := int(binary.BigEndian.Uint16(p[0:2]))
	p = p[2:]
	if len(p) < paLen {
		return
	}
	attrs := p[:paLen]

	var mpReach, mpUnreach []byte
	var rts []string
	for len(attrs) >= 3 {
		flags := attrs[0]
		atype := attrs[1]
		var alen, hdr int
		if flags&0x10 != 0 {
			alen = int(binary.BigEndian.Uint16(attrs[2:4]))
			hdr = 4
		} else {
			alen = int(attrs[2])
			hdr = 3
		}
		if len(attrs) < hdr+alen {
			break
		}
		val := attrs[hdr : hdr+alen]
		switch atype {
		case attrMPReach:
			mpReach = val
		case attrMPUnreach:
			mpUnreach = val
		case attrExtComm:
			rts = parseRTs(val)
		}
		attrs = attrs[hdr+alen:]
	}
	if mpReach != nil {
		b.handleMP(peer, mpReach, rts, false)
	}
	if mpUnreach != nil {
		b.handleMP(peer, mpUnreach, nil, true)
	}
}

func (b *BMP) handleMP(peer net.IP, v []byte, rts []string, withdraw bool) {
	if len(v) < 3 {
		return
	}
	afi := binary.BigEndian.Uint16(v[0:2])
	safi := v[2]
	if afi != afiIPv4 || safi != safiMPLSVPN {
		return // only VPN-IPv4 for now
	}
	p := v[3:]
	var nh net.IP
	if !withdraw {
		if len(p) < 1 {
			return
		}
		nhLen := int(p[0])
		p = p[1:]
		if len(p) < nhLen {
			return
		}
		if nhLen >= 12 { // VPN next hop = 8-byte RD (zero) + IPv4
			nh = net.IP(append([]byte(nil), p[8:12]...))
		}
		p = p[nhLen:]
		if len(p) < 1 {
			return
		}
		p = p[1:] // reserved SNPA
	}
	for len(p) > 0 {
		rd, prefix, label, n := parseVPNNLRI(p)
		if n == 0 {
			break
		}
		p = p[n:]
		b.writeVPNRoute(peer, rd, prefix, label, nh, rts, withdraw)
	}
}

// parseVPNNLRI decodes one labeled VPN-IPv4 NLRI: [len bits][label(s)][RD][prefix].
func parseVPNNLRI(p []byte) (rd, prefix string, label uint32, consumed int) {
	if len(p) < 1 {
		return "", "", 0, 0
	}
	bits := int(p[0])
	total := (bits + 7) / 8
	if len(p) < 1+total {
		return "", "", 0, 0
	}
	data := p[1 : 1+total]
	i := 0
	for i+3 <= len(data) { // 3-byte label stack entries until bottom-of-stack
		label = (uint32(data[i])<<16 | uint32(data[i+1])<<8 | uint32(data[i+2])) >> 4
		bos := data[i+2] & 1
		i += 3
		if bos == 1 {
			break
		}
	}
	if i+8 > len(data) {
		return "", "", 0, 0
	}
	rd = formatRD(data[i : i+8])
	i += 8
	ip := make(net.IP, net.IPv4len)
	copy(ip, data[i:])
	prefixBits := bits - i*8
	if prefixBits < 0 {
		prefixBits = 0
	}
	prefix = ip.String() + "/" + strconv.Itoa(prefixBits)
	return rd, prefix, label, 1 + total
}

func (b *BMP) writeVPNRoute(peer net.IP, rd, prefix string, label uint32, nh net.IP, rts []string, withdraw bool) {
	if withdraw {
		_ = b.c.Update("frr", nil, []*gnmipb.Path{{Elem: vpnRouteElems(rd, prefix, "")}})
		log.Printf("[bmp] WITHDRAW vpn rd=%s %s peer=%s", rd, prefix, peer)
		b.cor.Emit("bmp", "vpn-withdraw", prefix, "rd="+rd+" peer="+peer.String(), false)
		return
	}
	b.cor.Emit("bmp", "vpn-announce", prefix, "rd="+rd+" peer="+peer.String(), false)
	nhStr := "-"
	if nh != nil {
		nhStr = nh.String()
	}
	rtStr := strings.Join(rts, ",")
	ups := []*gnmipb.Update{
		leafUpdate(vpnRouteElems(rd, prefix, "label"), strconv.FormatUint(uint64(label), 10)),
		leafUpdate(vpnRouteElems(rd, prefix, "next-hop"), nhStr),
		leafUpdate(vpnRouteElems(rd, prefix, "route-target"), rtStr),
		leafUpdate(vpnRouteElems(rd, prefix, "peer"), peer.String()),
	}
	_ = b.c.Update("frr", ups, nil)
	log.Printf("[bmp] VPN rd=%s %s label=%d nh=%s rt=%s peer=%s", rd, prefix, label, nhStr, rtStr, peer)
}

// ---- helpers ----

func leafUpdate(elems []*gnmipb.PathElem, val string) *gnmipb.Update {
	return &gnmipb.Update{Path: &gnmipb.Path{Elem: elems}, Val: strVal(val)}
}

// openconfig: /network-instances/network-instance[name=vrf]/protocols/
//
//	protocol[identifier=BGP][name=bgp]/bgp/neighbors/neighbor[neighbor-address=peer]/state/<leaf>
func neighborElems(vrf, peer, leaf string) []*gnmipb.PathElem {
	return []*gnmipb.PathElem{
		{Name: "network-instances"},
		{Name: "network-instance", Key: map[string]string{"name": vrf}},
		{Name: "protocols"},
		{Name: "protocol", Key: map[string]string{"identifier": "BGP", "name": "bgp"}},
		{Name: "bgp"},
		{Name: "neighbors"},
		{Name: "neighbor", Key: map[string]string{"neighbor-address": peer}},
		{Name: "state"},
		{Name: leaf},
	}
}

// frr: /bgp-rib/afi-safis/afi-safi[name=l3vpn-ipv4-unicast]/routes/route[rd][prefix]/state/<leaf>
func vpnRouteElems(rd, prefix, leaf string) []*gnmipb.PathElem {
	e := []*gnmipb.PathElem{
		{Name: "bgp-rib"},
		{Name: "afi-safis"},
		{Name: "afi-safi", Key: map[string]string{"name": "l3vpn-ipv4-unicast"}},
		{Name: "routes"},
		{Name: "route", Key: map[string]string{"rd": rd, "prefix": prefix}},
	}
	if leaf != "" {
		e = append(e, &gnmipb.PathElem{Name: "state"}, &gnmipb.PathElem{Name: leaf})
	}
	return e
}

func formatRD(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	switch binary.BigEndian.Uint16(b[0:2]) {
	case 0:
		return strconv.Itoa(int(binary.BigEndian.Uint16(b[2:4]))) + ":" + strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[4:8])), 10)
	case 1:
		return net.IP(b[2:6]).String() + ":" + strconv.Itoa(int(binary.BigEndian.Uint16(b[6:8])))
	case 2:
		return strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[2:6])), 10) + ":" + strconv.Itoa(int(binary.BigEndian.Uint16(b[6:8])))
	default:
		return ""
	}
}

// parseRTs extracts route-targets (ext-comm subtype 0x02) from the attribute.
func parseRTs(v []byte) []string {
	var rts []string
	for len(v) >= 8 {
		c := v[:8]
		v = v[8:]
		if c[1] != 0x02 { // subtype: route target
			continue
		}
		switch c[0] {
		case 0x00: // 2-octet AS
			rts = append(rts, strconv.Itoa(int(binary.BigEndian.Uint16(c[2:4])))+":"+strconv.FormatUint(uint64(binary.BigEndian.Uint32(c[4:8])), 10))
		case 0x01: // IPv4
			rts = append(rts, net.IP(c[2:6]).String()+":"+strconv.Itoa(int(binary.BigEndian.Uint16(c[6:8]))))
		case 0x02: // 4-octet AS
			rts = append(rts, strconv.FormatUint(uint64(binary.BigEndian.Uint32(c[2:6])), 10)+":"+strconv.Itoa(int(binary.BigEndian.Uint16(c[6:8]))))
		}
	}
	return rts
}
