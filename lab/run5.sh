#!/bin/bash
# LLDP ingester test: l1 runs lldpd + the shim binary (same mount ns so lldpcli works),
# l2 is the LLDP neighbor. Verify /lldp neighbors via gNMI + ON_CHANGE on l2 stop.
IMG=router:v1
docker rm -f l1 l2 subw >/dev/null 2>&1
docker network rm lldpnet >/dev/null 2>&1
docker network create lldpnet >/dev/null
BR=br-$(docker network inspect lldpnet -f "{{.Id}}" | cut -c1-12)
sudo sh -c "echo 0x4000 > /sys/class/net/$BR/bridge/group_fwd_mask"

# l1 has the shim binary mounted; l2 is just a neighbor
docker run -d --name l1 --privileged --network lldpnet -v /tmp/frr-visible:/frr-visible:ro --entrypoint sh $IMG -c "sleep infinity" >/dev/null
docker run -d --name l2 --privileged --network lldpnet --entrypoint sh $IMG -c "sleep infinity" >/dev/null
for n in l1 l2; do
  docker exec $n apk add --no-cache lldpd >/dev/null 2>&1
  docker exec -d $n lldpd -d
done
sleep 2
for n in l1 l2; do docker exec $n lldpcli configure lldp tx-interval 2 >/dev/null 2>&1; done

echo "=== 在 l1 内跑 shim(lldp ingester 用本地 lldpcli)==="
docker exec -d l1 sh -c "/frr-visible -gnmi :9339 -fpm 127.0.0.1:2620 -bmp 127.0.0.1:5000 -target frr >/tmp/shim.log 2>&1"
sleep 6

L1IP=$(docker inspect -f "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}" l1)
echo "l1 IP=$L1IP"
echo "=== shim lldp 日志 ==="; docker exec l1 grep -E "\[lldp\]" /tmp/shim.log | head -8

echo ""
echo "=== gNMI 订阅 /lldp(在 l2 上跑 subtest 客户端)==="
docker cp /tmp/subtest l2:/subtest >/dev/null 2>&1
docker exec l2 /subtest -a $L1IP:9339 -target frr -origin openconfig -path lldp -once 2>&1 | grep -E "neighbor|chassis-id|system-name|port-id" | head -10

echo ""
echo "=== ON_CHANGE:STREAM 订阅,停 l2 lldpd 看邻居删除 ==="
docker exec -d l2 sh -c "/subtest -a $L1IP:9339 -target frr -origin openconfig -path lldp >/tmp/watch.log 2>&1"
sleep 2
docker exec l2 pkill lldpd
sleep 5
echo "-- 订阅端是否收到 DELETE? --"
docker exec l2 grep -E "DELETE.*lldp" /tmp/watch.log | head -3 && echo ">>> LLDP ON_CHANGE OK" || echo ">>> not seen"
echo "-- shim 端 lldp 删除日志 --"; docker exec l1 grep "neighbor DEL" /tmp/shim.log | head -2
