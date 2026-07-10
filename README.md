# frr-visible

给 FRR 容器体外套一层 **gNMI 壳**:事件驱动 ingester 把 FRR/内核状态灌进一棵 OpenConfig
cache,对外用 gNMI Subscribe 暴露。设计详见 `../frr-visible.md`。

## 当前进度:FPM(转发面)+ BMP(控制面),L3VPN 双视图已打通

```
zebra FPM(事件) ─┐
bgpd BMP(事件) ──┤→ ingesters → internal/state/cache.go(openconfig/gnmi cache,"cache 居中")
                  │            → internal/gnmiserver(subscribe.Server:ONCE + STREAM/ON_CHANGE)
                  │            → gNMI 客户端
```

同一条 VPN 路由,控制面 + 转发面在一棵 cache 对齐:
- 控制面(BMP):`frr:/bgp-rib/afi-safis/afi-safi[name=l3vpn-ipv4-unicast]/routes/route[rd][prefix]/state/{label,route-target,next-hop,peer}`
- 转发面(FPM):`openconfig:/network-instances/network-instance[name=<vrf>]/afts/ipv4-unicast/ipv4-entry[prefix]/state/next-hop`
- 邻居(BMP): `openconfig:/network-instances/.../bgp/neighbors/neighbor[neighbor-address]/state/{session-state,peer-as}`

已验证:VRF 名解析、nexthop 解析、RD/RT/label 提取、接口状态/计数/FDB、live 加路由秒级 ON_CHANGE。

指标覆盖(8/8 均有 ingester):1 CPU/内存 ✅ · 2 端口状态/流量 ✅ · 3 VLAN/FDB ✅ · 4 MPLS(FIB) · 5 OSPF ✅ · 6 BGP ✅ · 7 LLDP ✅ · 8 L3VPN ✅(控制面+转发面)。
三个 origin 均有真实数据:`openconfig`(接口/BGP/L3VPN-FIB/LLDP)· `frr`(L3VPN 控制面 RD/RT/label)· `host`(容器 CPU/内存)。

### 踩坑记录(部署/测试)
- **bind-mount 文件的 inode 坑**:`go build -o` 生成新 inode,`-v file:/x` 绑的是旧 inode,容器看不到新构建 → 改用 `docker cp` 更新容器内二进制。
- **lldpcli watch 块缓冲**:stdout 是管道时 libc 块缓冲,事件延迟 → 加 15s 周期 reconcile 兜底(LLDP 变化慢,足够)。
- LLDP 需与 lldpd 同 mount ns(lldpcli 走 Unix socket);测试时把 shim 二进制跑进带 lldpd 的容器内。
- **⚠️ /dev/log 回压死锁(重要)**:shim 绑 `/dev/log`(unix datagram,**可靠投递**)当 syslog 接收器,若读得慢(如对每条消息内联 fork vtysh),接收缓冲满 → FRR 的 `syslog()` **阻塞** → 拖垮所有 daemon(vtysh 挂死)。**监控壳绝不能拖垮被监控者**。修复:syslog 读循环只**持续排空** + 非阻塞发信号,独立 worker **去抖后**才 reconcile,并 `SetReadBuffer(1MB)`。**生产建议**:改用「FRR `log file` + inotify tail」——写方(FRR)追加文件永不阻塞,彻底消除回压风险(待做)。

## 目录

- `cmd/frr-visible` — 主程序(cache + gNMI server + FPM + BMP ingester)
- `cmd/subtest`    — 验证用 gNMI Subscribe 客户端(`-once`/STREAM,`-origin`,`-path`)
- `internal/state` — OpenConfig cache 封装
- `internal/gnmiserver` — gNMI 服务端:Subscribe(复用 openconfig subscribe)+ Get + Capabilities;Set 待做
- `internal/ingest/fpm.go` — FPM ingester(转发面:路由 + nexthop-group 解析)
- `internal/ingest/bmp.go` — BMP ingester(控制面:peer 状态 + VPNv4 路由/RD/RT/label)
- `internal/ingest/netlink.go` — netlink ingester(接口状态 ON_CHANGE / 计数 SAMPLE / FDB)
- `internal/ingest/lldp.go` — LLDP ingester(lldpcli watch 触发 + json reconcile + 15s 兜底)
- `internal/ingest/cgroup.go` — cgroup ingester(容器 CPU/内存,host origin,SAMPLE)
- `internal/ingest/ospf.go` — OSPF ingester(syslog /dev/log 触发 + vtysh reconcile,解耦排空+去抖)
- `internal/ingest/vrf.go` — VRF table→名 解析(netlink)

## 构建 / 运行(在 my-frr VM 内,Go 1.24+)

```bash
cd /Users/fanwei/arista/frr-visible
CGO_ENABLED=0 go build -o /tmp/frr-visible ./cmd/frr-visible
CGO_ENABLED=0 go build -o /tmp/subtest     ./cmd/subtest

# sidecar:与 FRR 共享 netns(方案 B),这样 netlink 能读到 VRF 设备
docker run -d --name shim --network container:pe1 --privileged \
  -v /tmp/frr-visible:/frr-visible:ro --entrypoint /frr-visible \
  alpine -gnmi :9339 -fpm 127.0.0.1:2620 -bmp 127.0.0.1:5000 -target frr

# FRR 侧把 FPM / BMP 指向本地(同 netns)
docker exec pe1 vtysh -c "conf t" -c "fpm address 127.0.0.1 port 2620"
docker exec pe1 vtysh -c "conf t" -c "router bgp 65000" -c "bmp targets T1" \
  -c "bmp connect 127.0.0.1 port 5000 min-retry 1000 max-retry 5000"

# 订阅验证:转发面(openconfig AFT)/ 控制面(frr bgp-rib)
docker run --rm --network pelab -v /tmp/subtest:/subtest:ro --entrypoint /subtest \
  alpine -a 172.30.0.11:9339 -target frr -origin openconfig -path network-instances -once
docker run --rm --network pelab -v /tmp/subtest:/subtest:ro --entrypoint /subtest \
  alpine -a 172.30.0.11:9339 -target frr -origin frr -path bgp-rib -once
```

## 已知 TODO

- ✅ ~~next-hop 解析~~:已跟踪 RTM_NEWNEXTHOP 对象 + RTA_NH_ID,解析真实下一跳 / blackhole。
- ✅ ~~VRF 名映射~~:sidecar 共享 netns 后用 vishvananda/netlink 读 VRF 设备,table id→名(cust/cust2)。
- **origin**:客户端需用 `origin=openconfig` 订阅(OC 树);`host`/`frr` 私有树待加。
- 仅 IPv4 路由 next-hop 单值落 cache;IPv6、多下一跳、MPLS-LFIB(AF_MPLS)、更多 AFT 字段待补。

## gNMI RPC 覆盖

- ✅ **Subscribe**(STREAM/ONCE + ON_CHANGE/SAMPLE)—— 流式遥测
- ✅ **Get**(一次性快照,三个 origin 均验证:openconfig/host/frr)—— gnmic 实测
- ✅ **Capabilities**(版本/模型/编码发现)—— gnmic 实测
- ⬜ **Set**(配置下发)—— 唯一剩余;写侧,风险高,从低风险子集起步

## 下一步

Set(配置下发)—— 从只读遥测走向可配置。
OSPF syslog 可选硬化为「log file + inotify」(消除 /dev/log 回压风险)。
