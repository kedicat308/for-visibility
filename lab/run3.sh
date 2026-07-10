#!/bin/bash
# Verify kernel-side event buses: netlink multicast (link/vlan/fdb/route/mpls) + lldpd watch.
cd /tmp/bmpfpm || exit 1
IMG=router:v1

echo "############ PART A: netlink multicast (link / VLAN / FDB / route) ############"
docker rm -f swx >/dev/null 2>&1
docker run -d --name swx --privileged --network none --entrypoint sh $IMG -c "sleep infinity" >/dev/null
docker exec swx sh -c '
ip link add br0 type bridge vlan_filtering 1; ip link set br0 up
ip link add swp1 type veth peer name h1; ip link add swp2 type veth peer name h2
ip link set swp1 master br0; ip link set swp2 master br0
ip link set swp1 up; ip link set swp2 up; ip link set h1 up; ip link set h2 up'
docker exec -d swx sh -c "ip -t monitor link route label > /tmp/nl.log 2>&1"
docker exec -d swx sh -c "bridge -t monitor > /tmp/br.log 2>&1"
sleep 1
echo "-- trigger: link down/up, add vlan100, add fdb, add route --"
docker exec swx sh -c '
ip link set swp1 down; sleep 0.3; ip link set swp1 up; sleep 0.3
bridge vlan add dev swp1 vid 100; sleep 0.3
bridge fdb add 00:11:22:33:44:55 dev swp1 vlan 100 master static; sleep 0.3
ip addr add 192.0.2.1/24 dev br0; ip route add 198.51.100.0/24 dev br0; sleep 0.3'
sleep 1
echo "===== ip monitor (link/route) ====="; docker exec swx cat /tmp/nl.log
echo "===== bridge monitor (fdb/vlan) ====="; docker exec swx cat /tmp/br.log

echo "############ PART A2: MPLS LFIB monitor (reuse pe1, VPN label churn) ############"
echo "-- LFIB before --"; docker exec pe1 ip -f mpls route show 2>&1 | head
docker exec -d pe1 sh -c "ip -f mpls -t monitor > /tmp/mpls.log 2>&1"
sleep 1
echo "-- trigger: add local VRF route 10.5.5.0/24 -> bgpd allocates new VPN label --"
docker exec pe1 vtysh -c "conf t" -c "vrf cust" -c "ip route 10.5.5.0/24 blackhole" >/dev/null 2>&1
sleep 2
echo "-- LFIB after --"; docker exec pe1 ip -f mpls route show 2>&1 | head
echo "-- mpls table --"; docker exec pe1 vtysh -c "show mpls table" 2>&1 | head
echo "===== ip -f mpls monitor ====="; docker exec pe1 cat /tmp/mpls.log

echo "############ PART B: lldpd watch (l1 <-> l2) ############"
docker rm -f l1 l2 >/dev/null 2>&1; docker network rm lldpnet >/dev/null 2>&1
docker network create lldpnet >/dev/null
BR=br-$(docker network inspect lldpnet -f "{{.Id}}" | cut -c1-12)
sudo sh -c "echo 0x4000 > /sys/class/net/$BR/bridge/group_fwd_mask" 2>&1
echo "LLDP fwd enabled on $BR: group_fwd_mask=$(cat /sys/class/net/$BR/bridge/group_fwd_mask 2>/dev/null)"
for n in l1 l2; do
  docker run -d --name $n --privileged --network lldpnet --entrypoint sh $IMG -c "sleep infinity" >/dev/null
  docker exec $n apk add --no-cache lldpd >/dev/null 2>&1
  docker exec -d $n lldpd -d
done
sleep 2
for n in l1 l2; do docker exec $n lldpcli configure lldp tx-interval 2 >/dev/null 2>&1; done
docker exec -d l1 sh -c "lldpcli watch > /tmp/watch.log 2>&1"
sleep 6
echo "-- l1 sees neighbor l2? --"; docker exec l1 lldpcli show neighbors 2>&1 | grep -Ei "SysName|PortID|ChassisID" | head
echo "-- trigger: stop lldpd on l2 (sends shutdown LLDPDU) --"
docker exec l2 pkill lldpd; sleep 4
echo "===== l1 lldpcli watch log (neighbor add + delete events) ====="; docker exec l1 cat /tmp/watch.log 2>&1 | head -50

echo "############ VERDICT ############"
echo -n "netlink link push:  "; docker exec swx grep -qiE "swp1" /tmp/nl.log && echo YES || echo NO
echo -n "netlink route push: "; docker exec swx grep -qE "198.51.100" /tmp/nl.log && echo YES || echo NO
echo -n "bridge vlan push:   "; docker exec swx grep -qiE "vlan 100|vid 100" /tmp/br.log && echo YES || echo NO
echo -n "bridge fdb push:    "; docker exec swx grep -qiE "00:11:22:33:44:55" /tmp/br.log && echo YES || echo NO
echo -n "mpls LFIB push:     "; docker exec pe1 test -s /tmp/mpls.log && echo YES || echo NO
echo -n "lldpd watch event:  "; docker exec l1 test -s /tmp/watch.log && echo YES || echo NO
echo "(extra containers swx/l1/l2 + net lldpnet left running)"
