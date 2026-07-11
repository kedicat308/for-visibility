// trace-aggregator stitches per-node convergence traces (each shim's :9340/traces)
// into cross-device "distributed traces": one topology event seen across every
// router it touched, as a single merged timeline.
//
// Routing has no propagated trace-id, so correlation is reconstructed: per-node
// traces whose start times fall within a window are grouped (a link flap is seen
// at both ends within sub-millisecond, and remote FIB churn follows within the
// window). Link endpoints (pe1-p1 <-> p1-pe1) normalize to one link as corroboration.
//
// Config via env:  INVENTORY node=mgmtIP,...   WINDOW (default 1.5s)   INTERVAL (3s)   LISTEN (:9341)
// Serves the distributed traces as JSON at /dtraces.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"frr-visible/internal/correlate"
)

type dspan struct {
	OffsetMs int64  `json:"off_ms"`
	Node     string `json:"node"`
	Bus      string `json:"bus"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Detail   string `json:"detail"`
	abs      time.Time
}

type dtrace struct {
	ID     int       `json:"id"`
	Start  time.Time `json:"start"`
	SpanMs int64     `json:"span_ms"`
	Link   string    `json:"link,omitempty"`
	Nodes  []string  `json:"nodes"`
	Roots  []string  `json:"roots"`
	Spans  []dspan   `json:"spans"`
}

var (
	inventory = map[string]string{}
	window    = 1500 * time.Millisecond
	interval  = 3 * time.Second
	listen    = ":9341"
	zipkinURL = "" // TEMPO_ZIPKIN, e.g. http://tempo:9411/api/v2/spans; empty disables export

	pushed = map[string]bool{} // trace_ids already exported to Tempo
	mu     sync.RWMutex
	out    []dtrace
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[trace-aggregator] ")
	inventory = parseInventory(os.Getenv("INVENTORY"))
	if v := os.Getenv("WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			window = d
		}
	}
	if v := os.Getenv("INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	if v := os.Getenv("LISTEN"); v != "" {
		listen = v
	}
	zipkinURL = os.Getenv("TEMPO_ZIPKIN")
	if len(inventory) == 0 {
		log.Fatal("need INVENTORY (node=mgmtIP,...)")
	}
	log.Printf("%d nodes, window=%s, interval=%s, listen=%s", len(inventory), window, interval, listen)

	go loop()
	http.HandleFunc("/dtraces", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	log.Fatal(http.ListenAndServe(listen, nil))
}

func loop() {
	cl := &http.Client{Timeout: 4 * time.Second}
	for {
		dt := build(cl)
		mu.Lock()
		out = dt
		mu.Unlock()
		if zipkinURL != "" {
			for i := range dt {
				pushZipkin(cl, dt[i])
			}
		}
		time.Sleep(interval)
	}
}

// build pulls every node's per-device traces and clusters them by start time.
func build(cl *http.Client) []dtrace {
	var all []correlate.Trace
	for node, ip := range inventory {
		addr := ip
		if !strings.Contains(addr, ":") {
			addr += ":9340"
		}
		for _, t := range fetch(cl, "http://"+addr+"/traces") {
			if t.Node == "" {
				t.Node = node
			}
			all = append(all, t)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Start.Before(all[j].Start) })

	var groups [][]correlate.Trace
	for _, t := range all {
		if n := len(groups); n > 0 && t.Start.Sub(groups[n-1][0].Start) <= window {
			groups[n-1] = append(groups[n-1], t)
		} else {
			groups = append(groups, []correlate.Trace{t})
		}
	}

	dts := make([]dtrace, 0, len(groups))
	for i, g := range groups {
		dts = append(dts, merge(i+1, g))
	}
	return dts
}

func merge(id int, g []correlate.Trace) dtrace {
	var spans []dspan
	nodeSet := map[string]bool{}
	var roots []string
	linkVotes := map[string]int{}
	for _, t := range g {
		nodeSet[t.Node] = true
		roots = append(roots, t.Node+": "+t.Root)
		if lk := linkKey(t.Root); lk != "" {
			linkVotes[lk]++
		}
		for _, s := range t.Spans {
			abs := t.Start.Add(time.Duration(s.OffsetMs) * time.Millisecond)
			spans = append(spans, dspan{
				Node: t.Node, Bus: s.Bus, Kind: s.Kind, Key: s.Key, Detail: s.Detail, abs: abs,
			})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].abs.Before(spans[j].abs) })
	start := time.Time{}
	if len(spans) > 0 {
		start = spans[0].abs
	}
	var span int64
	for i := range spans {
		spans[i].OffsetMs = spans[i].abs.Sub(start).Milliseconds()
		if spans[i].OffsetMs > span {
			span = spans[i].OffsetMs
		}
	}
	nodes := keys(nodeSet)
	sort.Strings(nodes)
	link := ""
	best := 0
	for k, v := range linkVotes {
		if v > best {
			best, link = v, k
		}
	}
	return dtrace{ID: id, Start: start, SpanMs: span, Link: link, Nodes: nodes, Roots: roots, Spans: spans}
}

// linkKey normalizes a link-event root ("netlink/link-down pe1-p1") to a
// direction-independent key ("p1--pe1"); returns "" for non-link roots.
func linkKey(root string) string {
	i := strings.Index(root, "link-")
	if i < 0 {
		return ""
	}
	f := strings.Fields(root[i:])
	if len(f) < 2 {
		return ""
	}
	iface := f[1] // e.g. pe1-p1
	parts := strings.SplitN(iface, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	a, b := parts[0], parts[1]
	if a > b {
		a, b = b, a
	}
	return a + "--" + b
}

func fetch(cl *http.Client, url string) []correlate.Trace {
	resp, err := cl.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ts []correlate.Trace
	_ = json.Unmarshal(body, &ts)
	return ts
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func parseInventory(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// ---- Tempo export (Zipkin v2 spans; no OTel SDK needed) ----

type zEndpoint struct {
	ServiceName string `json:"serviceName"`
}

type zSpan struct {
	TraceID       string            `json:"traceId"`
	ID            string            `json:"id"`
	ParentID      string            `json:"parentId,omitempty"`
	Name          string            `json:"name"`
	Timestamp     int64             `json:"timestamp"` // epoch micros
	Duration      int64             `json:"duration"`  // micros
	LocalEndpoint zEndpoint         `json:"localEndpoint"`
	Tags          map[string]string `json:"tags,omitempty"`
}

func hexID(seed string, n int) string {
	h := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(h[:n])
}

// pushZipkin exports one distributed trace to Tempo as Zipkin v2 spans, exactly
// once per trace_id. The dtrace becomes a root span; each hop becomes a child
// span timestamped at its absolute time and tagged with its owning node — so
// Grafana renders the cross-device waterfall natively.
func pushZipkin(cl *http.Client, dt dtrace) {
	seed := dt.Start.Format(time.RFC3339Nano) + "|" + dt.Link + "|" + strings.Join(dt.Roots, ";")
	traceID := hexID(seed, 16) // 16 bytes -> 32 hex
	if pushed[traceID] {
		return
	}
	rootID := hexID(traceID+"|root", 8)
	name := "convergence"
	if dt.Link != "" {
		name += " " + dt.Link
	}
	spans := []zSpan{{
		TraceID: traceID, ID: rootID, Name: name,
		Timestamp: dt.Start.UnixMicro(), Duration: dt.SpanMs*1000 + 1,
		LocalEndpoint: zEndpoint{ServiceName: "network"},
		Tags:          map[string]string{"link": dt.Link, "nodes": strings.Join(dt.Nodes, ","), "roots": strings.Join(dt.Roots, " | ")},
	}}
	for i, s := range dt.Spans {
		abs := dt.Start.Add(time.Duration(s.OffsetMs) * time.Millisecond)
		dur := int64(1000)
		if i+1 < len(dt.Spans) {
			if g := (dt.Spans[i+1].OffsetMs - s.OffsetMs) * 1000; g > dur {
				dur = g
			}
		}
		spans = append(spans, zSpan{
			TraceID: traceID, ID: hexID(traceID+"|"+strconv.Itoa(i), 8), ParentID: rootID,
			Name:      s.Bus + "/" + s.Kind,
			Timestamp: abs.UnixMicro(), Duration: dur,
			LocalEndpoint: zEndpoint{ServiceName: s.Node},
			Tags:          map[string]string{"key": s.Key, "detail": s.Detail},
		})
	}
	b, _ := json.Marshal(spans)
	resp, err := cl.Post(zipkinURL, "application/json", bytes.NewReader(b))
	if err != nil {
		log.Printf("zipkin push %s: %v", traceID, err)
		return
	}
	resp.Body.Close()
	pushed[traceID] = true
	log.Printf("exported dtrace #%d -> tempo (trace_id=%s, %d spans)", dt.ID, traceID, len(spans))
}
