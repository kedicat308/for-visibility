// Package correlate reconstructs a "convergence trace": the causal timeline of a
// routing event (a link/adjacency change) as it ripples across a router's internal
// buses — netlink, OSPF syslog, FPM (zebra FIB), BMP (bgpd) — within one node.
//
// Routing protocols carry no trace-id / context propagation (unlike an HTTP
// traceparent header), so causality here is RECONSTRUCTED, not propagated: a
// "root" event (link-down/up, adjacency-down/up) opens a trace window, and every
// follow-up event (route add/del, VPN withdraw/announce) that lands within the
// window is attached as a span. Follow-up events with no active root are dropped —
// which conveniently filters the startup full-sync (all follow, no root).
//
// This is the honest, single-node approximation. Cross-device stitching (join
// per-node traces by prefix/time) and OTLP export to Tempo/Jaeger come next.
package correlate

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// Event is one thing that happened on one bus. Root marks a topology trigger
// (link/adjacency state change) that may start a new trace.
type Event struct {
	Bus, Kind, Key, Detail string
	Root                   bool
	TS                     time.Time
}

// Span is an Event placed on a trace's timeline (offset from the root).
type Span struct {
	OffsetMs int64  `json:"off_ms"`
	Bus      string `json:"bus"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Detail   string `json:"detail"`
}

// Trace is one convergence event: a root plus the spans it caused.
type Trace struct {
	Node   string    `json:"node"`
	ID     int       `json:"id"`
	Root   string    `json:"root"`
	Start  time.Time `json:"start"`
	SpanMs int64     `json:"span_ms"`
	Spans  []Span    `json:"spans"`
}

// Correlator serially folds a stream of Events into Traces.
type Correlator struct {
	node   string
	ch     chan Event
	window time.Duration // max gap for an event to join the active trace
	idle   time.Duration // flush the active trace after this much quiet

	mu   sync.Mutex
	ring []Trace // most-recent-last, capped
	max  int
	seq  int
}

func New(node string) *Correlator {
	return &Correlator{
		node:   node,
		ch:     make(chan Event, 1024),
		window: 3 * time.Second,
		idle:   3 * time.Second,
		max:    50,
	}
}

// Emit is nil-safe and non-blocking: an ingester may call it unconditionally and
// it will never block the caller (a full buffer drops the event, logged once).
func (c *Correlator) Emit(bus, kind, key, detail string, root bool) {
	if c == nil {
		return
	}
	select {
	case c.ch <- Event{Bus: bus, Kind: kind, Key: key, Detail: detail, Root: root, TS: time.Now()}:
	default:
		log.Printf("[correlate] drop event (buffer full): %s/%s %s", bus, kind, key)
	}
}

type acc struct {
	start, last time.Time
	root        string
	events      []Event
}

// Run consumes events and flushes traces. Blocking; call in its own goroutine.
func (c *Correlator) Run() {
	var active *acc
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case e := <-c.ch:
			if active != nil && e.TS.Sub(active.last) > c.window {
				c.flush(active)
				active = nil
			}
			if active == nil {
				if !e.Root {
					continue // a follow-up with no active root: ignore (filters full-sync)
				}
				active = &acc{start: e.TS, last: e.TS, root: e.Bus + "/" + e.Kind + " " + e.Key}
			}
			active.last = e.TS
			active.events = append(active.events, e)
		case now := <-tick.C:
			if active != nil && now.Sub(active.last) > c.idle {
				c.flush(active)
				active = nil
			}
		}
	}
}

func (c *Correlator) flush(a *acc) {
	if a == nil || len(a.events) == 0 {
		return
	}
	c.seq++
	spans := make([]Span, len(a.events))
	for i, e := range a.events {
		spans[i] = Span{
			OffsetMs: e.TS.Sub(a.start).Milliseconds(),
			Bus:      e.Bus, Kind: e.Kind, Key: e.Key, Detail: e.Detail,
		}
	}
	t := Trace{
		Node:   c.node,
		ID:     c.seq,
		Root:   a.root,
		Start:  a.start,
		SpanMs: a.last.Sub(a.start).Milliseconds(),
		Spans:  spans,
	}
	b, _ := json.Marshal(t)
	log.Printf("[trace] %s", b)

	c.mu.Lock()
	c.ring = append(c.ring, t)
	if len(c.ring) > c.max {
		c.ring = c.ring[len(c.ring)-c.max:]
	}
	c.mu.Unlock()
}

// ServeHTTP returns the recent traces as a JSON array (newest last).
func (c *Correlator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	out := make([]Trace, len(c.ring))
	copy(out, c.ring)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
