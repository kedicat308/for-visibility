// ospf.go covers OSPF neighbor adjacency (metric 5). FRR has no clean external
// event bus for OSPF, so we use the design's chosen source: syslog. The shim
// binds /dev/log (unix datagram) and FRR `log syslog` PUSHES adjacency-change
// messages to it — event-driven, not polling. Each OSPF message triggers a
// reconcile via `vtysh show ip ospf neighbor json` (accurate structured state;
// syslog is only the "something changed" trigger, same pattern as lldp watch).
// A one-time snapshot runs at startup; an optional low-frequency reconcile can
// be enabled as a safety net (syslog is lossy) but defaults off for pure push.
package ingest

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"

	"frr-visible/internal/state"
)

type OSPF struct {
	c        *state.Cache
	vtysh    string
	sock     string
	fallback time.Duration
	seen     map[string]string // router-id -> interface (for deletes)
}

func NewOSPF(c *state.Cache, fallback time.Duration) *OSPF {
	return &OSPF{c: c, vtysh: "vtysh", sock: "/dev/log", fallback: fallback, seen: map[string]string{}}
}

func (o *OSPF) Run() error {
	o.reconcile() // startup snapshot
	trigger := make(chan struct{}, 1)
	go o.runSyslog(trigger)
	go o.reconcileWorker(trigger)
	if o.fallback > 0 {
		go func() {
			t := time.NewTicker(o.fallback)
			defer t.Stop()
			for range t.C {
				signal(trigger)
			}
		}()
	}
	select {}
}

// runSyslog binds /dev/log and ONLY drains it, signalling the worker on OSPF
// messages. It must never block on reconcile: /dev/log is a unix datagram socket
// with reliable delivery, so a slow reader back-pressures FRR's syslog() and can
// wedge the daemons. Draining continuously + a debounced worker keeps FRR safe —
// the monitor must never harm the monitored (cf. CoPP).
func (o *OSPF) runSyslog(trigger chan<- struct{}) {
	_ = os.Remove(o.sock)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: o.sock, Net: "unixgram"})
	if err != nil {
		log.Printf("[ospf] syslog bind %s failed (snapshot/fallback only): %v", o.sock, err)
		return
	}
	_ = os.Chmod(o.sock, 0666)
	_ = conn.SetReadBuffer(1 << 20) // absorb adjacency bursts
	log.Printf("[ospf] syslog receiver on %s (configure FRR: log syslog informational)", o.sock)
	buf := make([]byte, 8192)
	for {
		n, _, err := conn.ReadFromUnix(buf) // always draining
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(buf[:n])), "ospf") {
			signal(trigger)
		}
	}
}

// reconcileWorker coalesces a burst of triggers into one debounced reconcile,
// bounding how often we fork vtysh.
func (o *OSPF) reconcileWorker(trigger <-chan struct{}) {
	for range trigger {
		time.Sleep(300 * time.Millisecond) // let the adjacency burst settle
		o.reconcile()
	}
}

// signal does a non-blocking send (coalesce: at most one pending trigger).
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (o *OSPF) reconcile() {
	out, err := exec.Command(o.vtysh, "-c", "show ip ospf neighbor json").Output()
	if err != nil {
		return
	}
	cur := parseOSPFNbrs(out)

	seen := map[string]string{}
	for rid, n := range cur {
		seen[rid] = n.iface
		ups := []*gnmipb.Update{
			leafUpdate(ospfNbrElems(n.iface, rid, "adjacency-state"), adjState(n.state)),
			leafUpdate(ospfNbrElems(n.iface, rid, "neighbor-address"), n.addr),
		}
		_ = o.c.Update("openconfig", ups, nil)
		log.Printf("[ospf] nbr %s if=%s state=%s", rid, n.iface, adjState(n.state))
	}
	for rid, iface := range o.seen {
		if _, ok := seen[rid]; !ok {
			_ = o.c.Update("openconfig", nil, []*gnmipb.Path{{Elem: ospfNbrElems(iface, rid, "")}})
			log.Printf("[ospf] nbr DEL %s", rid)
		}
	}
	o.seen = seen
}

type ospfNbr struct {
	routerID, iface, addr, state string
}

func parseOSPFNbrs(data []byte) map[string]*ospfNbr {
	res := map[string]*ospfNbr{}
	var root struct {
		Neighbors map[string][]struct {
			NbrState     string `json:"nbrState"`
			Converged    string `json:"converged"`
			IfaceName    string `json:"ifaceName"`
			IfaceAddress string `json:"ifaceAddress"`
		} `json:"neighbors"`
	}
	if json.Unmarshal(data, &root) != nil {
		return res
	}
	for rid, arr := range root.Neighbors {
		if len(arr) == 0 {
			continue
		}
		n := arr[0]
		iface := n.IfaceName
		if i := strings.IndexByte(iface, ':'); i >= 0 { // "eth0:172.30.0.11" -> "eth0"
			iface = iface[:i]
		}
		state := n.Converged
		if state == "" {
			state = n.NbrState
		}
		res[rid] = &ospfNbr{routerID: rid, iface: iface, addr: n.IfaceAddress, state: state}
	}
	return res
}

// adjState maps FRR state to the OpenConfig adjacency-state enum.
func adjState(s string) string {
	switch strings.SplitN(s, "/", 2)[0] {
	case "Full":
		return "FULL"
	case "2-Way":
		return "TWO_WAY"
	case "Init":
		return "INIT"
	case "Down":
		return "DOWN"
	case "ExStart":
		return "EXCHANGE_START"
	case "Exchange":
		return "EXCHANGE"
	case "Loading":
		return "LOADING"
	case "Attempt":
		return "ATTEMPT"
	default:
		return "UNKNOWN"
	}
}

// openconfig: /network-instances/network-instance[name=default]/protocols/
//   protocol[OSPF]/ospfv2/areas/area[0.0.0.0]/interfaces/interface[id]/neighbors/
//   neighbor[router-id]/state/<leaf>   (area defaulted to 0.0.0.0; single-area lab)
func ospfNbrElems(iface, rid, leaf string) []*gnmipb.PathElem {
	e := []*gnmipb.PathElem{
		{Name: "network-instances"},
		{Name: "network-instance", Key: map[string]string{"name": "default"}},
		{Name: "protocols"},
		{Name: "protocol", Key: map[string]string{"identifier": "OSPF", "name": "ospf"}},
		{Name: "ospfv2"},
		{Name: "areas"},
		{Name: "area", Key: map[string]string{"identifier": "0.0.0.0"}},
		{Name: "interfaces"},
		{Name: "interface", Key: map[string]string{"id": iface}},
		{Name: "neighbors"},
		{Name: "neighbor", Key: map[string]string{"router-id": rid}},
	}
	if leaf != "" {
		e = append(e, &gnmipb.PathElem{Name: "state"}, &gnmipb.PathElem{Name: leaf})
	}
	return e
}
