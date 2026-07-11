#!/usr/bin/env bash
# check-topo.sh — verify the 5-node backbone converged. Run inside my-frr VM.
set -uo pipefail
line() { printf '\n=== %s ===\n' "$1"; }

line "OSPF neighbors (pe1 / p1 / pe2 — expect FULL)"
for n in pe1 p1 pe2; do
  echo "-- $n"; docker exec "$n" vtysh -c "show ip ospf neighbor" 2>/dev/null | tail -n +2
done

line "LDP neighbors (expect OPERATIONAL)"
for n in pe1 p1 pe2; do
  echo "-- $n"; docker exec "$n" vtysh -c "show mpls ldp neighbor" 2>/dev/null | grep -E "OPERATIONAL|NON EXISTENT|Peer" || echo "  (none)"
done

line "iBGP VPNv4 (pe1 <-> pe2 — expect Established + prefixes)"
docker exec pe1 vtysh -c "show bgp ipv4 vpn summary" 2>/dev/null | grep -A3 Neighbor || echo "  (no vpn af)"

line "PE-CE eBGP in VRF cust (pe1<-ce1, pe2<-ce2)"
docker exec pe1 vtysh -c "show bgp vrf cust ipv4 summary" 2>/dev/null | grep -A3 Neighbor
docker exec pe2 vtysh -c "show bgp vrf cust ipv4 summary" 2>/dev/null | grep -A3 Neighbor

line "L3VPN reachability: ce1 -> ce2 loopback (10.0.0.102) across the VPN"
docker exec ce1 ping -c2 -W2 10.0.0.102 2>&1 | tail -n3

line "VRF FIB on pe1 (expect ce2's 10.0.0.102 via MPLS label)"
docker exec pe1 vtysh -c "show ip route vrf cust 10.0.0.102/32" 2>/dev/null | grep -E "10.0.0.102|label|via" | head
