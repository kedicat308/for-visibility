#!/usr/bin/env bash
# build-topo.sh — reproducible 5-node FRR MPLS L3VPN backbone for the frr-visible shim.
#
#   ce1 ─── pe1 ─── p1 ─── pe2 ─── ce2
#           └───── VRF cust ──────┘        iBGP VPNv4 pe1<->pe2
#   OSPF area0 + LDP on the core (pe1-p1-pe2); eBGP PE-CE (ce1/ce2)
#
# Each node also has a leg on campus-mgmt (172.30.30.0/24) so the shim's
# gNMI :9339 is reachable by the gnmic collector. Runs INSIDE the my-frr VM.
#
# Idempotent: tears down any prior run of these names first.
set -euo pipefail

IMG=router:v1
NODES="ce1 pe1 p1 pe2 ce2"

# ---- data-plane p2p networks (one docker bridge per link) --------------------
declare -A NETS=(
  [l_ce1pe1]=10.1.0.0/24
  [l_pe1p1]=10.2.0.0/24
  [l_p1pe2]=10.3.0.0/24
  [l_pe2ce2]=10.4.0.0/24
)

echo "== teardown any prior run =="
for n in $NODES; do docker rm -f "$n" >/dev/null 2>&1 || true; done
for net in "${!NETS[@]}"; do docker network rm "$net" >/dev/null 2>&1 || true; done

echo "== create data-plane networks =="
for net in "${!NETS[@]}"; do
  # park the bridge gateway at .254 so .1/.2 are free for the routers
  gw="${NETS[$net]%.0/24}.254"
  docker network create --subnet "${NETS[$net]}" --gateway "$gw" "$net" >/dev/null
  echo "   $net ${NETS[$net]} gw=$gw"
done

# ---- launch containers (privileged; MPLS platform labels; just sleep) --------
# frr is started later via exec, after kernel plumbing (vrf/mpls/loopback) is done.
run() { # name  first-net  ip
  docker run -d --name "$1" --hostname "$1" --privileged \
    --network "$2" --ip "$3" "$IMG" \
    sh -c "sysctl -w net.mpls.platform_labels=100000 >/dev/null 2>&1; sleep infinity" >/dev/null
}
echo "== launch containers =="
run ce1 l_ce1pe1 10.1.0.1
run pe1 l_ce1pe1 10.1.0.2
run p1  l_pe1p1  10.2.0.2
run pe2 l_p1pe2  10.3.0.2
run ce2 l_pe2ce2 10.4.0.2

echo "== attach remaining legs =="
docker network connect l_pe1p1  pe1 --ip 10.2.0.1
docker network connect l_p1pe2  p1  --ip 10.3.0.1
docker network connect l_pe2ce2 pe2 --ip 10.4.0.1
# management legs
docker network connect campus-mgmt ce1 --ip 172.30.30.101
docker network connect campus-mgmt pe1 --ip 172.30.30.111
docker network connect campus-mgmt p1  --ip 172.30.30.112
docker network connect campus-mgmt pe2 --ip 172.30.30.113
docker network connect campus-mgmt ce2 --ip 172.30.30.102

# ---- helper: interface name carrying a given IPv4 ---------------------------
ifof() { docker exec "$1" ip -o -4 addr show | awk -v p="$2/" 'index($4,p)==1{print $2; exit}'; }

echo "== resolve interface names =="
CE1_UP=$(ifof ce1 10.1.0.1)                       # ce1 -> pe1
PE1_CE=$(ifof pe1 10.1.0.2); PE1_CORE=$(ifof pe1 10.2.0.1)
P1_A=$(ifof p1 10.2.0.2);    P1_B=$(ifof p1 10.3.0.1)
PE2_CORE=$(ifof pe2 10.3.0.2); PE2_CE=$(ifof pe2 10.4.0.1)
CE2_UP=$(ifof ce2 10.4.0.2)                       # ce2 -> pe2
echo "   pe1: ce=$PE1_CE core=$PE1_CORE | p1: a=$P1_A b=$P1_B | pe2: core=$PE2_CORE ce=$PE2_CE"

# ---- kernel plumbing: loopbacks, VRF on PEs, MPLS input on core --------------
lo() { docker exec "$1" ip addr add "$2/32" dev lo 2>/dev/null || true; }
mplsin() { docker exec "$1" sysctl -w "net.mpls.conf.$2.input=1" >/dev/null 2>&1 || true; }

echo "== kernel plumbing =="
lo ce1 10.0.0.101; lo pe1 10.0.0.1; lo p1 10.0.0.2; lo pe2 10.0.0.3; lo ce2 10.0.0.102

# VRF cust (table 100) on the PEs; move the CE-facing leg in and re-add its IP
for pe in pe1 pe2; do
  docker exec "$pe" ip link add cust type vrf table 100 2>/dev/null || true
  docker exec "$pe" ip link set cust up
done
# enslaving to a VRF keeps the IPv4 addr on this kernel; re-add only if it was flushed
docker exec pe1 sh -c "ip link set $PE1_CE master cust; ip addr add 10.1.0.2/24 dev $PE1_CE 2>/dev/null || true"
docker exec pe2 sh -c "ip link set $PE2_CE master cust; ip addr add 10.4.0.1/24 dev $PE2_CE 2>/dev/null || true"

# MPLS input on core-facing interfaces (labeled traffic ingress)
mplsin pe1 "$PE1_CORE"
mplsin p1 "$P1_A"; mplsin p1 "$P1_B"
mplsin pe2 "$PE2_CORE"

# ---- enable daemons ----------------------------------------------------------
DAEMONS='zebra=yes
bgpd=yes
ospfd=yes
ldpd=yes
staticd=yes
mgmtd=yes
vtysh_enable=yes
zebra_options="  -A 127.0.0.1 -s 90000000 -M dplane_fpm_nl"
bgpd_options="   -A 127.0.0.1 -M bmp"
ospfd_options="  -A 127.0.0.1"
ldpd_options="   -A 127.0.0.1"'
for n in $NODES; do
  docker exec "$n" sh -c "printf '%s\n' \"\$0\" > /etc/frr/daemons" "$DAEMONS"
  docker exec "$n" sh -c "echo 'service integrated-vtysh-config' > /etc/frr/vtysh.conf"
done

# ---- per-node integrated frr.conf -------------------------------------------
push() { docker exec -i "$1" sh -c "cat > /etc/frr/frr.conf"; }

echo "== render frr.conf =="

push ce1 <<EOF
hostname ce1
!
interface lo
 ip address 10.0.0.101/32
!
router bgp 65101
 bgp router-id 10.0.0.101
 no bgp ebgp-requires-policy
 neighbor 10.1.0.2 remote-as 65000
 address-family ipv4 unicast
  redistribute connected
 exit-address-family
!
EOF

push pe1 <<EOF
hostname pe1
!
interface lo
 ip address 10.0.0.1/32
!
interface $PE1_CORE
 ip ospf network point-to-point
!
router ospf
 ospf router-id 10.0.0.1
 network 10.0.0.1/32 area 0
 network 10.2.0.0/24 area 0
 log-adjacency-changes detail
!
router bgp 65000
 bgp router-id 10.0.0.1
 neighbor 10.0.0.3 remote-as 65000
 neighbor 10.0.0.3 update-source lo
 address-family ipv4 unicast
  no neighbor 10.0.0.3 activate
 exit-address-family
 address-family ipv4 vpn
  neighbor 10.0.0.3 activate
 exit-address-family
!
router bgp 65000 vrf cust
 bgp router-id 10.0.0.1
 no bgp ebgp-requires-policy
 neighbor 10.1.0.1 remote-as 65101
 address-family ipv4 unicast
  neighbor 10.1.0.1 activate
  redistribute connected
  label vpn export auto
  rd vpn export 65000:1
  rt vpn both 65000:1
  export vpn
  import vpn
 exit-address-family
!
EOF

push p1 <<EOF
hostname p1
!
interface lo
 ip address 10.0.0.2/32
!
interface $P1_A
 ip ospf network point-to-point
!
interface $P1_B
 ip ospf network point-to-point
!
router ospf
 ospf router-id 10.0.0.2
 network 10.0.0.2/32 area 0
 network 10.2.0.0/24 area 0
 network 10.3.0.0/24 area 0
 log-adjacency-changes detail
!
EOF

push pe2 <<EOF
hostname pe2
!
interface lo
 ip address 10.0.0.3/32
!
interface $PE2_CORE
 ip ospf network point-to-point
!
router ospf
 ospf router-id 10.0.0.3
 network 10.0.0.3/32 area 0
 network 10.3.0.0/24 area 0
 log-adjacency-changes detail
!
router bgp 65000
 bgp router-id 10.0.0.3
 neighbor 10.0.0.1 remote-as 65000
 neighbor 10.0.0.1 update-source lo
 address-family ipv4 unicast
  no neighbor 10.0.0.1 activate
 exit-address-family
 address-family ipv4 vpn
  neighbor 10.0.0.1 activate
 exit-address-family
!
router bgp 65000 vrf cust
 bgp router-id 10.0.0.3
 no bgp ebgp-requires-policy
 neighbor 10.4.0.2 remote-as 65102
 address-family ipv4 unicast
  neighbor 10.4.0.2 activate
  redistribute connected
  label vpn export auto
  rd vpn export 65000:2
  rt vpn both 65000:1
  export vpn
  import vpn
 exit-address-family
!
EOF

push ce2 <<EOF
hostname ce2
!
interface lo
 ip address 10.0.0.102/32
!
router bgp 65102
 bgp router-id 10.0.0.102
 no bgp ebgp-requires-policy
 neighbor 10.4.0.1 remote-as 65000
 address-family ipv4 unicast
  redistribute connected
 exit-address-family
!
EOF

# ---- start FRR ---------------------------------------------------------------
echo "== start FRR =="
for n in $NODES; do
  docker exec "$n" /usr/lib/frr/frrinit.sh start >/dev/null 2>&1 || \
    docker exec "$n" /usr/lib/frr/frrinit.sh restart >/dev/null 2>&1 || true
done

# ---- LDP: apply to the running ldpd via vtysh (more reliable than the
#      integrated-config load order, which can race ldpd startup) --------------
echo "== configure LDP on the core =="
ldp() { # node  router-id  coreif...
  local n=$1 id=$2; shift 2
  local cmds=(-c 'configure terminal' -c 'mpls ldp' -c " router-id $id" -c ' address-family ipv4' -c "  discovery transport-address $id")
  local i; for i in "$@"; do cmds+=(-c "  interface $i"); done
  docker exec "$n" vtysh "${cmds[@]}" >/dev/null 2>&1 || true
}
sleep 2
ldp pe1 10.0.0.1 "$PE1_CORE"
ldp p1  10.0.0.2 "$P1_A" "$P1_B"
ldp pe2 10.0.0.3 "$PE2_CORE"

echo "== done. give OSPF/LDP/BGP ~40s to converge, then check-topo.sh =="
