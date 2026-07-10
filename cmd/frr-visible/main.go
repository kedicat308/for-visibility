// Command frr-visible is the gNMI shim: it runs event-driven ingesters that
// feed a shared OpenConfig cache, and serves gNMI Subscribe over it.
// v0 wires just the FPM ingester (route/FIB) — the shortest closed loop.
package main

import (
	"flag"
	"log"
	"net"
	"time"

	"frr-visible/internal/gnmiserver"
	"frr-visible/internal/ingest"
	"frr-visible/internal/state"
)

func main() {
	gnmiAddr := flag.String("gnmi", ":9339", "gNMI Subscribe listen address")
	fpmAddr := flag.String("fpm", ":2620", "FPM listen address (zebra dials in here)")
	bmpAddr := flag.String("bmp", ":5000", "BMP listen address (bgpd dials in here)")
	target := flag.String("target", "frr", "gNMI cache target name")
	ospfReconcile := flag.Duration("ospf-reconcile", 0, "OSPF safety-net reconcile interval (0=off, pure event-driven)")
	flag.Parse()

	c := state.New(*target)

	// gNMI Subscribe server over the cache.
	grpcSrv, err := gnmiserver.New(c)
	if err != nil {
		log.Fatalf("gnmi server: %v", err)
	}
	lis, err := net.Listen("tcp", *gnmiAddr)
	if err != nil {
		log.Fatalf("gnmi listen %s: %v", *gnmiAddr, err)
	}
	go func() {
		log.Printf("[gnmi] Subscribe server on %s (target=%q)", *gnmiAddr, *target)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Fatalf("gnmi serve: %v", err)
		}
	}()

	// BMP ingester (BGP/L3VPN control plane).
	bmp := ingest.NewBMP(*bmpAddr, c)
	go func() {
		if err := bmp.Run(); err != nil {
			log.Fatalf("bmp: %v", err)
		}
	}()

	// Netlink ingester (interfaces/VLAN/FDB from the kernel).
	nl := ingest.NewNetlink(c, 10*time.Second)
	go func() {
		if err := nl.Run(); err != nil {
			log.Printf("netlink: %v", err)
		}
	}()

	// LLDP ingester (neighbors via lldpd, if present).
	lldp := ingest.NewLLDP(c)
	go func() {
		if err := lldp.Run(); err != nil {
			log.Printf("lldp: %v", err)
		}
	}()

	// Cgroup ingester (container CPU/memory).
	cg := ingest.NewCgroup(c, 10*time.Second)
	go func() {
		if err := cg.Run(); err != nil {
			log.Printf("cgroup: %v", err)
		}
	}()

	// OSPF ingester (neighbor adjacency via syslog trigger + vtysh reconcile).
	ospf := ingest.NewOSPF(c, *ospfReconcile)
	go func() {
		if err := ospf.Run(); err != nil {
			log.Printf("ospf: %v", err)
		}
	}()

	// FPM ingester (route/FIB, blocks).
	fpm := ingest.NewFPM(*fpmAddr, c)
	if err := fpm.Run(); err != nil {
		log.Fatalf("fpm: %v", err)
	}
}
