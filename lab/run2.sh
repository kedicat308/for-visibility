#!/bin/bash
# MPLS L3VPN functional test: OSPF+LDP underlay, iBGP VPNv4, cust VRF on each PE.
# PE1 runs BMP(monitor ipv4 vpn)+FPM -> verify VPN routes(+label) via BMP, VRF FIB via FPM.
cd /tmp/bmpfpm || exit 1
NET=pelab; SUB=172.30.0.0/24; GW=172.30.0.1; IMG=router:v1

echo "########## 0. cleanup + kernel MPLS on host ##########"
docker rm -f pe1 pe2 >/dev/null 2>&1
pkill -f collector.py 2>/dev/null; sleep 1
sudo modprobe mpls_router mpls_iptunnel 2>&1 | head -2
docker network inspect $NET >/dev/null 2>&1 || docker network create --subnet $SUB --gateway $GW $NET

echo "########## 1. daemons (ospfd/ldpd/bgpd/staticd + -M bmp/-M dplane_fpm_nl) ##########"
docker run --rm --entrypoint cat $IMG /etc/frr/daemons > daemons.base
sed -e 's|^zebra_options=.*|zebra_options="  -A 127.0.0.1 -s 90000000 -M dplane_fpm_nl"|' \
    -e 's|^bgpd_options=.*|bgpd_options="   -A 127.0.0.1 -M bmp"|' daemons.base > daemons
grep -q '^staticd=yes' daemons || echo 'staticd=yes' >> daemons

echo "########## 2. collectors (BMP :5000, FPM :2620) ##########"
setsid python3 collector.py BMP 5000 bmp > bmp.log 2>&1 < /dev/null &
setsid python3 collector.py FPM 2620 fpm > fpm.log 2>&1 < /dev/null &
sleep 1

echo "########## 3. launch PEs (create cust VRF + mpls sysctls, then FRR) ##########"
START='ip link add cust type vrf table 100 2>/dev/null; ip link set cust up;
sysctl -w net.mpls.platform_labels=100000 >/dev/null 2>&1;
sysctl -w net.mpls.conf.eth0.input=1 >/dev/null 2>&1;
/usr/lib/frr/frrinit.sh start && sleep infinity'
for pe in pe1 pe2; do
  ip=11; [ $pe = pe2 ] && ip=12
  docker run -d --name $pe --hostname $pe --network $NET --ip 172.30.0.$ip --privileged \
    -v "$PWD/daemons:/etc/frr/daemons:ro" -v "$PWD/$pe-vpn.conf:/etc/frr/frr.conf:ro" \
    --entrypoint sh $IMG -c "$START" >/dev/null
done
echo "waiting for OSPF + LDP + iBGP-VPNv4 convergence..."; sleep 28

echo "########## 4. PE1 underlay ##########"
echo "-- OSPF neighbor --"; docker exec pe1 vtysh -c "show ip ospf neighbor" 2>&1 | grep -E "2.2.2.2|Neighbor"
echo "-- LDP neighbor --";  docker exec pe1 vtysh -c "show mpls ldp neighbor" 2>&1 | grep -E "2.2.2.2|OPER|Peer" | head
echo "-- VPNv4 BGP summary --"; docker exec pe1 vtysh -c "show bgp ipv4 vpn summary" 2>&1 | grep -E "2.2.2.2|Neighbor|Estab"

echo "########## 5. metric-8 payload on PE1 ##########"
echo "-- VPNv4 table (expect RD 65000:2 10.2.2.0/24 with a label) --"
docker exec pe1 vtysh -c "show bgp ipv4 vpn" 2>&1 | grep -E "Route Distinguisher|10.2.2.0|10.1.1.0"
echo "-- detail of learned VPN route (RD/RT/label) --"
docker exec pe1 vtysh -c "show bgp ipv4 vpn 10.2.2.0/24" 2>&1 | grep -Ei "Distinguisher|label|Extended|import|remote" | head
echo "-- VRF cust FIB (expect 10.2.2.0/24 via 2.2.2.2, local 10.1.1.0/24) --"
docker exec pe1 vtysh -c "show ip route vrf cust" 2>&1 | grep -E "10.1.1.0|10.2.2.0|^B|^S"
echo "-- MPLS table (transport + VPN labels) --"
docker exec pe1 vtysh -c "show mpls table" 2>&1 | head -15

echo "########## 6. collector logs ##########"
echo "===== BMP (expect RouteMonitoring for VPNv4) ====="; cat bmp.log
echo "===== FPM (expect VRF routes / MPLS label entries) ====="; tail -25 fpm.log

echo "########## 7. live change: add VPN route on PE2 ##########"
docker exec pe2 vtysh -c "conf t" -c "vrf cust" -c "ip route 10.9.9.0/24 blackhole" 2>&1
sleep 5
echo "-- PE1 VRF sees 10.9.9.0/24? --"; docker exec pe1 vtysh -c "show ip route vrf cust 10.9.9.0/24" 2>&1 | head -4
echo "===== BMP tail ====="; tail -6 bmp.log
echo "===== FPM tail ====="; tail -6 fpm.log

echo "########## 8. verdict ##########"
echo -n "iBGP VPNv4 up: "; docker exec pe1 vtysh -c "show bgp ipv4 vpn summary" 2>&1 | grep -q "2.2.2.2.*[0-9]" && echo YES || echo NO
echo -n "VPN route learned in VRF FIB: "; docker exec pe1 vtysh -c "show ip route vrf cust" 2>&1 | grep -q "10.2.2.0" && echo YES || echo NO
echo -n "BMP streamed VPNv4: "; grep -q RouteMonitoring bmp.log && echo YES || echo NO
echo -n "FPM streamed routes: "; grep -q RTM_NEWROUTE fpm.log && echo YES || echo NO
