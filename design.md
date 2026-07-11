# frr-visible —— 让 FRR 容器"看起来像一台路由交换机"的设计总结

> 目标:用 Go 在 FRR 容器**体外/带外**实现一个 gNMI 壳(shim),对外把这个容器
> 呈现成一台**标准路由交换机**——既能被 gNMI/OpenConfig collector 订阅遥测(主动推),
> 又能通过 gNMI Set 接收配置。对内 FRR 只干它擅长的三层控制面,二层与系统指标由内核和其它进程提供,
> 壳负责把它们统一成一张 OpenConfig 的脸。
>
> 本文是前期设计讨论的完整结论,后续开发以此为准。

---

## 0. 一句话结论

**不要改 FRR。** 正确形态是"**Linux 路由交换机(内核交换 + FRR 路由 + lldpd/mstpd)+ 一个 cache 居中的 gNMI 壳**"。
对外(gNMI/OpenConfig)它是一台路由交换机;对内是若干独立进程各司其职,壳在最外层统一呈现。
这是 Cumulus Linux / SONiC 走了十年、被验证过的路子,本项目是它的**小型化复刻**。

**核心决策(采集机制):事件驱动优先。** 状态类全部走事件总线 push(netlink 组播 / FPM / BMP / syslog / lldpd-watch),
**只有物理上没有"变化事件"的 gauge 类(接口计数、CPU/内存、前缀数)才 SAMPLE**——与 Arista/Cisco 同级别。
不做"轮询 + diff 假装 on-change"。详见 §3 / §12.5。

---

## 1. 需要采集/暴露的 8 类指标 → 真实数据源(设计地基)

关键认知:**这 8 类指标来自至少 3 个不同的面,FRR 只覆盖一半左右。这个壳本质是"多源采集器",不是"问 FRR 要一切"。**

| # | 指标 | 真正的数据源 | 采集方式 | 是否 FRR |
|---|------|------------|---------|---------|
| 1 | 容器 CPU/内存 | cgroup | 读 `/sys/fs/cgroup`(v2: `cpu.stat` / `memory.current` / `memory.max`) | ❌ 内核 |
| 2 | 端口状态/流量 | 内核 | **netlink** `RTM_GETLINK` stats64(rx/tx bytes/pkts/err/drop)+ link 事件订阅 | ❌ 内核 |
| 3 | VLAN | 内核 | netlink(AF_BRIDGE:bridge vlan 表 / FDB;802.1Q 子接口计数) | ❌ 内核 |
| 4 | MPLS | 内核 + ldpd | LFIB=**netlink AF_MPLS 组播(事件)**;L3 路由=**FPM(事件,已验证)**;LDP 邻居走 ldpd(syslog 事件)。**注:per-label 计数器内核支持很弱** | ⚠️ 一半 |
| 5 | OSPF | ospfd | 邻居变化=**syslog / SNMP-trap(事件)**;LSDB/SPF 细节=`show ... json`(SAMPLE 兜底) | ✅ FRR |
| 6 | BGP | bgpd | peer 状态 + 路由=**BMP(事件推送)**;summary/前缀数=`show ... json`(SAMPLE 兜底) | ✅ FRR |
| 7 | LLDP | **lldpd** | `lldpctl -f json`(**FRR 根本不做 LLDP**,容器需另装 lldpd) | ❌ 独立进程 |
| 8 | VPN(**MPLS L3VPN**,RFC 4364)| bgpd(VPNv4/VPNv6)+ ldpd + 内核 LFIB | VPN 路由 + peer=**BMP(事件)**;VRF FIB=**FPM/netlink(事件)**;RD/RT/label 补充=`show bgp ipv4 vpn json`(SAMPLE)| ⚠️ 控制面 FRR,数据面内核 |

数据面划分小结:
- 指标 1/2/3 主要走 **netlink + cgroup**,直接问内核比问 FRR 更快更准。
- 指标 7(LLDP)FRR 里没有,必须另装 **lldpd**。
- 指标 8(VPN)**已定性 = 基于 MPLS 的传统三层 VPN(RFC 4364:MP-BGP VPNv4/VPNv6 + VRF + PE-CE + LDP 传输标签)**,不是 EVPN、不是 IPsec。它与指标 4(MPLS)、6(BGP)大量重叠,并把 **VRF 变成一等维度**(详见 §5.4)。
- **采集机制已决策:事件驱动优先**——netlink 组播 / FPM / BMP / syslog / lldpd-watch 全部走 push,只有计数器、CPU/内存、前缀数这类 gauge(物理上无事件)才 SAMPLE。详见 §12.5。

---

## 2. 架构:cache 居中的 gNMI 壳

```
        ┌──────────────── Go gNMI Shim (和 FRR 同 netns / pidns) ─────────────────┐
采集器 ──gNMI──▶│  gNMI Server: Capabilities / Get / Set / Subscribe                    │
(gnmic /    │        │                                                                     │
 collector) │        ▼            openconfig/gnmi cache (ctree, 全状态树)                  │
        │   Subscribe ◀── 从 cache 生成 notification ──┐                                   │
        │   Set ──▶ 翻译成配置(L3=vtysh -c / L2=bridge·ip / mgmtd) │                       │
        │                                              ▲ 各采集器写 cache                  │
        │   netlink事件/轮询 · cgroup · bgpd/ospfd .vty · lldpctl · mstpctl                │
        └──────────────────────────────────────────────────────────────────────────────────┘
```

**核心设计原则:cache 居中(state-store-centric)。** 各数据源采集器把状态写进一棵 OpenConfig 路径树(cache),
gNMI Subscribe 只从 cache 读并生成 notification。这样订阅逻辑与采集逻辑解耦,采集慢也不影响订阅输出。

> **这不是自创,是业界收敛结论**:SONiC=Redis(COUNTERS_DB/STATE_DB)、Arista EOS=Sysdb、
> SR Linux=中央 datastore、Cisco/Juniper=内部 sensor 总线。你的 cache 就是你的小号 Redis/Sysdb。

---

## 3. gNMI 层设计要点

- **proto**:`github.com/openconfig/gnmi/proto/gnmi`
- **Subscribe(遥测主体)**:直接复用 `github.com/openconfig/gnmi/cache` + `github.com/openconfig/gnmi/subscribe`
  的 `subscribe.Server`——它已实现 Subscribe RPC(STREAM/ONCE/POLL、ON_CHANGE/SAMPLE),只需往 cache 灌数据。**别自己撸订阅状态机。**
- **Get / Set / Capabilities**:自己实现这三个 RPC。Set 里把 gNMI path+val 翻译成设备配置。
- **schema / 编码**:`github.com/openconfig/ygot` 生成 OpenConfig Go 结构体,保证 path/类型合法。
- **测试 / 对端**:用 `gnmic` 当 collector 订阅验证。

**采集机制(已决策:事件驱动优先,不做"轮询 + diff 假装 on-change"):**
1. **状态类一律走事件总线,源变化即 push → 写 cache → gNMI on-change**:
   - 接口 / VLAN / FDB / MPLS-LFIB → **netlink 组播**(内核 push,`RTNLGRP_LINK/NEIGH/IPV4_ROUTE`、`AF_BRIDGE`、`AF_MPLS`)
   - L3 路由 / FIB / VPN 路由 → **FPM**(zebra push,Netlink-over-TCP,**已验证**)
   - BGP peer up/down + 路由 → **BMP**(bgpd push 给壳内置的 BMP station)
   - OSPF 邻居变化 → **syslog / SNMP-trap**(ospfd push)
   - LLDP 邻居变化 → **lldpd watch**(liblldpctl 订阅,push)
2. **只有 gauge 类才 SAMPLE**(物理上没有"变化事件"):接口计数、CPU/内存、前缀数。Arista/Cisco 同样 SAMPLE,这不是妥协。
3. **`vtysh`/`.vty` json 退居"冷补充/初始快照"**:仅用于事件总线拿不到的静态细节(SPF 统计、RD/RT、启动时全量快照),**低频、不做高频状态轮询**——呼应"读=CoPP 限流"。

**dial-in vs dial-out:**
- 标准 gNMI 是 dial-in(collector 连 target 发 Subscribe),够用就用它。
- "主动发送"若要 gNMI **dial-out**(target 主动连 collector 推 stream,和 Arista dial-out 对齐),数据模型完全一样,
  只是把 Subscribe 的 stream 方向反过来 + 外面加连接管理,cache 那套不变。

---

## 4. 部署形态

**硬约束:壳必须与 FRR 共享 network namespace,并能访问 FRR runtime 目录。** 它要碰的 4 样东西:

| 需要访问 | 约束 |
|---------|------|
| netlink(接口/VLAN/MPLS/路由/FDB) | **必须在 FRR 的 netns 里**,否则看到别的网卡 |
| FRR `.vty` socket(`/var/run/frr/*.vty`) | 要能读 FRR runtime 目录 |
| lldpd / mstpd 的 socket | 共享文件系统 |
| 容器 cgroup | 要读到 FRR 容器那个 cgroup |

→ 排除"跑在宿主机当独立进程"(netns 不对)。剩两个方案,**agent 代码完全一样,只是打包/编排不同**:

### 方案 A:装进同一个容器(embedded)——推荐先用
- Go 静态二进制(`CGO_ENABLED=0`,~10MB)加进镜像,用 supervisord/s6 同时拉起 `frrinit` 和 agent。
- 👍 一个容器搞定,天然共享 netns/fs/cgroup。 👎 改了官方镜像;两进程一容器隔离差。
- **适合:containerlab / 实验室 / 快速验证。**

### 方案 B:独立 sidecar 容器,共享命名空间——正式部署
- agent 单独镜像,和 FRR 容器 `network_mode: "container:frr"`(或 K8s 同 Pod)共享 **netns + pidns**,
  FRR 的 `/var/run/frr` 用 volume 挂进来。
- 👍 不动 FRR 官方镜像,各自升级;进程隔离干净。 👎 netns 共享 + volume 挂载要配对。
- **⚠️ cgroup 坑**:sidecar 读到的是**自己**的 cgroup,不是 FRR 容器的。需共享 pidns 后按 FRR 的 PID 找它的 cgroup 路径,
  或挂宿主 `/sys/fs/cgroup` 按 FRR 容器 ID 定位。(方案 A 无此问题,同一 cgroup。)

**建议:方案 A 跑通逻辑 → 定型后切方案 B 上正式环境。**

---

## 5. 让容器"看起来像一台路由交换机"

### 5.1 关键认知:FRR 不做二层

- FRR 是**纯三层控制面**:无桥接/交换、无自有 MAC 表、无 MAC 学习、**无 STP**、不处理 VLAN 本身。
- 真正的二层数据面 = **Linux 内核 bridge + netlink(AF_BRIDGE)**(或硬件 switchdev/SAI)。
- **唯一例外:EVPN 控制面**。bgpd+zebra 实现 BGP-EVPN——zebra 从内核 bridge FDB 学 MAC → 通过 EVPN 广告;
  收到的再下发回内核 FDB/VXLAN。**实际桥接/封装仍是内核干,FRR 只是"控制面信使"。**

**别把二层转发塞进 FRR**:那是数据面功能,进用户态 = 性能崩塌 + 重造内核 bridge。业界从来是"FRR 配一个转发面",不是"把转发塞进 FRR"。

### 5.2 用 Linux 构件把"路由交换机"拼出来

| 路由交换机特性 | Linux 构件(容器内) | 谁提供 |
|---------------|-------------------|--------|
| 交换 fabric / L2 | **一个 VLAN-aware bridge**(`vlan_filtering 1`) | 内核 |
| switchport(access/trunk) | 口加入 bridge + `bridge vlan add vid` | 内核 |
| VLAN | bridge VLAN 表 | 内核 |
| SVI / VLAN 三层口 | bridge 上建 vlan 设备给 IP,FRR 在上面跑 | 内核 + FRR |
| 路由口(routed port) | 不入 bridge、直接给 IP 的口 | 内核 + FRR |
| VLAN 间路由 | 内核 FIB(FRR 灌) | FRR + 内核 |
| L3 路由协议 | bgpd / ospfd | FRR |
| LAG / 端口聚合 | bonding / team | 内核 |
| STP / RSTP / MSTP | **mstpd** | 独立进程 |
| LLDP | **lldpd** | 独立进程 |
| Overlay(VXLAN L2 延伸) | vxlan 设备 + **BGP-EVPN** | 内核 + FRR |
| VRF | Linux VRF + FRR | 内核 + FRR |

**最小骨架(示意):**
```bash
# 交换 fabric:一个 VLAN-aware bridge
ip link add name br0 type bridge vlan_filtering 1
# swp1 当 access VLAN10,swp2 当 trunk(10,20)
ip link set swp1 master br0 && bridge vlan add dev swp1 vid 10 pvid untagged
ip link set swp2 master br0 && bridge vlan add dev swp2 vid 10 && bridge vlan add dev swp2 vid 20
# SVI:VLAN10 的三层网关,FRR 在它上面跑 OSPF/BGP
ip link add link br0 name vlan10 type vlan id 10
ip addr add 10.0.10.1/24 dev vlan10
# routed port:swp3 直接三层
ip addr add 10.0.30.1/24 dev swp3
```
到这一步容器**已是一台能交换、能路由、能 VLAN 间路由、能跑 OSPF/BGP 的路由交换机,FRR 一行没改。**

### 5.3 "看起来像"这层 = 壳 + OpenConfig 映射

**OpenConfig 本来就是照"路由交换机"建模的**,壳把上面 Linux 状态映射到 OC 路径,collector 眼里它即一台标准路由交换机:

| 概念 | OpenConfig 路径 | 壳从哪采 |
|------|----------------|---------|
| switchport(access/trunk) | `/interfaces/interface/ethernet/switched-vlan/config/{interface-mode,access-vlan,trunk-vlans}` | netlink bridge vlan |
| VLAN 列表 | `/network-instances/.../vlans/vlan` | netlink |
| SVI / 三层口 | `/interfaces/interface[vlanXX]` + `.../routed-vlan` | netlink |
| MAC 表 | `/network-instances/.../fdb/mac-table` | netlink AF_BRIDGE |
| STP | `/stp/...` | mstpd(`mstpctl`) |
| LLDP 邻居 | `/lldp/interfaces/...` | lldpd(`lldpctl -f json`) |
| L3 路由/协议 | `/network-instances/.../protocols/{BGP,OSPF}` | FRR `.vty` socket |
| 端口/计数/状态 | `/interfaces/interface/state/counters`,`oper-status` | netlink |
| MPLS | `/network-instances/.../mpls/{lsps,signaling-protocols/ldp}` | netlink(AF_MPLS)+ ldpd |
| CPU/内存 | `/components/component/.../{cpu,memory}`(或自定义 origin) | cgroup |

**结果:对外 gNMI/OpenConfig 是路由交换机;对内是"内核 bridge + FRR + lldpd/mstpd + 壳"。**

**配置下发(Set)分面:** L3 → `vtysh -c`(BGP/OSPF)或 mgmtd(仅已迁 daemon);L2(VLAN/switchport/bridge)→ `bridge`/`ip` 命令或 netlink 写。

> 注:§5 里的 **EVPN** 指的是"二层 over VXLAN 的叠加"(让容器像交换机时的 L2 overlay 选项),
> 与**指标 8 的 MPLS L3VPN 是两回事**——一个是 L2VPN 控制面,一个是 L3VPN。本项目指标 8 只做后者(见 §5.4)。

### 5.4 MPLS L3VPN(指标 8)—— VRF 作为一等维度

指标 8 定性为 **RFC 4364 传统三层 VPN**:MP-BGP VPNv4/VPNv6 + Linux VRF + RD/RT + PE-CE + LDP 传输标签。
**FRR 完整支持**:bgpd 的 VPNv4/VPNv6 地址族做 VPN 路由分发与标签分配,zebra 把 MPLS LFIB 灌进内核,
传输 LSP 用 ldpd(LDP)。→ **容器进程清单必须启用 `ldpd`**(传输标签也可用 BGP-LU/SR,但传统 L3VPN 通常 LDP)。

**数据源(全部按 VRF 组织):**

| L3VPN 元素 | 数据源 | 采集命令 |
|-----------|--------|---------|
| VPNv4/VPNv6 路由、RD、RT、VPN 标签、条数 | bgpd | `show bgp ipv4 vpn json` / `show bgp ipv6 vpn json` |
| per-VRF RIB | zebra/bgpd | `show ip route vrf <vrf> json` / `show bgp vrf <vrf> ipv4 unicast json` |
| PE-CE 邻居(BGP/OSPF/static per VRF)| bgpd/ospfd | `show bgp vrf <vrf> summary json` / `show ip ospf vrf <vrf> neighbor json` |
| 传输 LSP / LDP 邻居 / 标签绑定 | ldpd | `show mpls ldp neighbor json` / `show mpls ldp binding json` |
| MPLS LFIB(传输 + VPN 标签)| 内核 | netlink **AF_MPLS** |

**架构影响(重要):VRF 成为一等维度。** 指标 2(接口)、5(OSPF)、6(BGP)全部要**按 VRF 拆**——
每个 VRF 有自己的接口集合、路由表、邻居。这天然对应 OpenConfig 的 **network-instance**,壳的 cache 树要以 network-instance 为顶层组织。

**OpenConfig 映射(network-instance type = L3VRF):**

| 概念 | OpenConfig 路径 |
|------|----------------|
| VRF 实例 + RD | `/network-instances/network-instance[NAME]/config/{type=L3VRF, route-distinguisher}` |
| RT 导入/导出 | `.../network-instance/inter-instance-policies/...` 或 l3vpn 扩展(OC 覆盖不全,见下) |
| VPN 路由 / 标签 | `.../protocols/protocol[BGP]/bgp/rib/afi-safis/afi-safi[l3vpn-ipv4-unicast]/...` |
| per-VRF 路由表 | `.../afts/...`(per network-instance) |
| MPLS 传输/LDP | `/network-instances/.../mpls/signaling-protocols/ldp/...`、`.../lsps/...` |
| 接口归属 VRF | `.../network-instance/interfaces/interface` |

**⚠️ 注意:OpenConfig 对 L3VPN / RT / label 的建模是较复杂、覆盖不全的区域。** VPN 指标优先用**自定义 origin 兜底**,
不要陷进"把 OC 的 l3vpn 树填全"的无底洞(呼应 §8「窄而够用」、§11 路径模型建议)。

---

## 6. 容器内进程清单 + 许可证

| 进程 | 职责 | 来源 | 许可证 |
|------|------|------|--------|
| frr(zebra/bgpd/ospfd/**ldpd**… + EVPN) | 三层控制面 + MPLS L3VPN(bgpd VPNv4/v6 + ldpd LDP)+ EVPN 控制面 | FRRouting(`github.com/FRRouting/frr`,Linux Foundation) | **GPL-2.0** |
| lldpd | LLDP(802.1AB,兼容 CDP/EDP/…) | Vincent Bernat(`github.com/lldpd/lldpd`) | **ISC** |
| mstpd | RSTP/MSTP(内核 bridge 只有基础 STP) | 社区(`github.com/mstpd/mstpd`) | **GPL-2.0** |
| gNMI 壳 | 多源采集 + gNMI 呈现 + 配置下发 | 你自己 | 随你 |

- lldpd 有 **JSON 输出(`lldpctl -f json`)和 SNMP(AgentX)**,采集最省事;是业界事实标准(Cumulus 等在用)。替代品 `open-lldp`/lldpad(Intel,偏 DCBX/FCoE)一般用不到。
- mstpd 只**跑协议算法**,再通过 `mstpctl`/netlink 告诉内核 bridge 各端口 forwarding/blocking——和"FRR 跑协议、内核转发"同一分工。
- **许可证纪律:壳与所有进程只通过 socket / CLI / gRPC 通信(不静态链接),因此不受 GPL 传染**,壳可自选许可证。
  这正是 `frr_exporter` 能用 Apache-2.0 的原因(外部进程连 FRR socket)。本设计天然满足——别把 FRR/mstpd 的 GPL 代码静态编进壳。

---

## 7. 业界对标(证明方向正确 + 可复用清单)

**"基于 FRR 监控"是主流,一大堆;但"基于 FRR 直接出 gNMI 的轻量 on-box 壳"几乎空白——这就是本项目的缝。**

| 类型 | 代表 | 怎么做 | 对本项目 |
|------|------|--------|---------|
| 白盒 NOS | **SONiC** | 状态进 Redis,`sonic-gnmi` 读 Redis 出遥测 | gNMI-over-状态存储的完整范例,**抄 Subscribe 思路** |
| FRR 系 NOS | **Cumulus(NVIDIA)** | 每节点跑 **NetQ agent**(内核+FRR+LLDP 采集,gRPC 推) | **on-box agent 形态原型** |
| 纯 FRR | 社区 | **`frr_exporter`**(scrape `.vty` json 出 Prometheus)+ node_exporter + lldpd | **抄它的 BGP/OSPF/route socket 解析逻辑** |
| BIRD 系 | 云/IX 软路由 | bird_exporter / birdwatcher | 参考 |
| VPP/DPDK | Cisco VPP | stats 共享内存段 + `vpp_prometheus_export` | 参考 |
| on-box(投资公司)| **telerista(IMC Trading)** | Telegraf 跑在 Arista 本机,采 Linux+LANZ+gNMI 接口 → InfluxDB | **验证 on-box Go agent 形态生产可用** |
| gNMI 客户端/导出 | **goarista(Arista 官方)** | `ocprometheus`/`ockafka`/`octsdb` 订 gNMI → Prometheus/Kafka/TSDB;含可复用 gNMI 客户端 | **抄 gNMI path→指标 的映射/客户端代码** |
| 商用 NOS(对照) | Arista/Cisco/Juniper/Nokia | 原生流式遥测(gNMI/MDT/JTI),中央状态存储 | 目标形态 |

**共同规律(全部指向本设计):**
1. **状态存储/cache 居中 + 遥测壳读它**,不是让 gNMI 直接戳每个进程。
2. **计数器来自转发面(内核/ASIC),不来自路由进程。**
3. **LLDP 永远独立(lldpd)。**
4. 拿路由协议状态有两条路,业界正从①迁向②:① CLI/socket 抓取(frr_exporter,今天能用但脆、压控制面);② 结构化 northbound(SONiC Redis / FRR mgmtd)。

**复用清单(自己只写"胶水 + 映射"):**
- 采集解析 ← `frr_exporter`(FRR socket → BGP/OSPF/route)
- gNMI 服务端 ← `openconfig/gnmi` 的 cache + subscribe;思路参考 `sonic-gnmi`
- gNMI 客户端 / path 映射 ← `goarista`
- LLDP ← `lldpctl -f json`
- **等于**:把 frr_exporter 的采集后端从"吐 Prometheus"换成"喂进 gNMI cache",即成型。

---

## 8. 为什么业界没人做"FRR 轻量 gNMI 壳"(市场结构,不是技术难)

1. **供需两拨人不重叠**:裸用 FRR 的(软路由/中小/云主机 BGP/实验室)只需 SNMP/Prometheus,不要 gNMI;
   要 gNMI 的(大运营商/超大规模 DC)买原生 gNMI 设备或上 SONiC,不裸用 FRR。交集太小,养不起项目。
2. **gNMI 价值只在"跨厂商统一"时显现**:只有 FRR。 Prometheus 就够,gNMI 是过度设计。
   只有**混合舰队(既有 Arista 又有 FRR)**才真需要"让 FRR 也说 gNMI"——**这正是本项目场景,但市场里是少数派**。
3. **OpenConfig YANG ↔ FRR 映射是持续脏活**:难在把 FRR 状态准确塞进深层 OC YANG 树,还要跟 OC / FRR 版本演进;
   比 frr_exporter 的扁平 metric 难一个量级。开源没动力,商业觉得市场太小。
4. **FRR 官方走自己的 mgmtd(不是 gNMI)**:社区精力在 mgmtd,没人做 gNMI 中间态。
5. **SONiC 已占住"FRR+gNMI"格子**:够大的组织直接上 SONiC,又吸走一批潜在用户。

**对本项目的判断:** 作为"通用开源产品"ROI 差(所以没人做);作为"自己混合舰队统一到 gNMI"的方案完全成立。
**正确姿势:做一个"窄而够用"的壳——只映射真正要的指标 OC 路径,其余用自定义 origin 兜底,别追全量 OC 覆盖。**

---

## 9. FRR mgmtd 现状(截至 FRR 10.x,当前 stable ~10.5)

**框架已成型,但迁移逐个 daemon 手工挪、进度慢,且 config 与 operational 两条线严重不均衡。**

| 项 | 状态 |
|---|---|
| 框架 | ✅ 集中式管理面、YANG 数据存储、candidate/running、事务提交、gRPC 前端 |
| 配置文件 | ✅ FRR 10.0 起只认统一 `frr.conf`,不再支持 per-daemon 配置 |
| 前端 | 现=gRPC + CLI;NETCONF/RESTCONF 是"将来可加",还没有 |

**daemon 迁移进度(决定能否用):**

| daemon | 迁到 mgmtd? | 对本项目 |
|--------|-----------|---------|
| staticd | ✅ 首个 | — |
| isisd | ✅ | — |
| zebra(接口等) | ⚠️ 部分 | — |
| **bgpd** | ❌ **未迁**(路线图 #5428 连负责人都没有;YANG 文件有了但 northbound 回调没接) | ✗ 指标 6/8 用不上 |
| **ospfd** | ❌ 未迁 | ✗ 指标 5 用不上 |
| pbrd | ❌ 未迁 | — |

**最要命:mgmtd 重心在 config,而本项目要的是 operational state(遥测)——恰是最弱、最没迁的那条线。**
官方原话:"许多 daemon 的 operational CLI 尚未 YANG 化。"

**对本项目的结论:**
1. **别指望用 mgmtd 拿 BGP/OSPF 遥测——现在拿不到。** 采集层老实抄 `frr_exporter` 连 `.vty` socket 抓 `show ... json`,短期不会变。
2. **配置下发(Set)分情况**:静态路由/IS-IS 可走 mgmtd gRPC;**BGP/OSPF 配置仍走 `vtysh -c` / 改 `frr.conf` + reload**。
3. **把 mgmtd 当"未来顺风车",不是现在的地基**;盯 #5428,等 bgpd northbound 回调接上、operational YANG 补齐再切采集后端。从"无负责人"看,乐观也得等挺久,项目不能押在它身上。

---

## 10. 落地路线(分阶段)

1. **数据面搭建(纯配置,不写代码)**:containerlab 里给 FRR 节点内建 VLAN-aware bridge + 两个 SVI + 一个 routed port + OSPF,
   验证"能 VLAN 间路由 + 能跑 OSPF/BGP"。
2. **补齐控制面**:容器并上 `lldpd`;需 STP 并 `mstpd`;EVPN 用 FRR 自带。
3. **壳 v0(最短闭环)——✅ 已实现并打磨(2026-07-10,代码在 `frr-visible/`)**:**FPM ingester → OpenConfig cache → gNMI Subscribe**。Go(openconfig/gnmi cache + subscribe.Server)。实测 default + L3VPN(cust/cust2)VRF 路由经 `/network-instances/network-instance[name=<vrf>]/afts/ipv4-unicast/ipv4-entry[…]/state/next-hop` 暴露,**VRF 名**(netlink 读 VRF 设备)、**next-hop**(RTM_NEWNEXTHOP 对象 + RTA_NH_ID 解析,识别 blackhole)均已解析;live 加路由秒级 ON_CHANGE。**部署已采用 sidecar(`--network container:pe1`,方案 B),shim 与 FRR 同 netns**,为 netlink ingester 铺路。剩:host/frr origin、IPv6/AF_MPLS。
4. **壳 v1(接齐事件总线)**:
   - ✅ **BMP ingester 已实现(2026-07-10)**:内置 BMP station,peer up/down → `openconfig:` 邻居 session-state;Route Monitoring 解析 MP_REACH VPNv4 → `frr:/bgp-rib/.../route[rd][prefix]` 带 label/route-target/next-hop/peer。**与 FPM 在同一 cache 对齐成 L3VPN 控制面+转发面双视图**,live 加路由秒级 ON_CHANGE 实测通过。
   - ✅ **netlink ingester 已实现(2026-07-10)**:link 事件 → `openconfig:/interfaces/interface[name]/state/{admin,oper}-status`(ON_CHANGE);计数 10s 轮询 → `.../state/counters/*`(SAMPLE);bridge FDB → `.../fdb/mac-table`(ON_CHANGE)。复用 `vishvananda/netlink`,实测接口状态/计数/FDB 均经 gNMI 暴露。覆盖指标 2/3。
   - ✅ **netlink ingester 已实现(2026-07-10)**:接口状态 ON_CHANGE、计数 SAMPLE、FDB。覆盖指标 2/3。
   - ✅ **LLDP ingester 已实现(2026-07-11)**:`lldpcli watch` 触发 + `lldpcli -f json show neighbors` reconcile + 15s 周期兜底(watch 块缓冲)→ `openconfig:/lldp/interfaces/interface[name]/neighbors/neighbor[id]/state/{chassis-id,port-id,system-name,system-description,management-address}`。实测邻居 5 leaf 经 gNMI 暴露、停 lldpd 秒级 DELETE。覆盖指标 7。
   - ✅ **cgroup ingester 已实现(2026-07-11)**:读 `/sys/fs/cgroup`(v2)cpu.stat + memory.* → `host:/container/{cpu,memory}/state/*`(SAMPLE 10s)。覆盖指标 1。**三个 origin(openconfig/host/frr)均有真实数据,数据模型闭环。**
   - ✅ **OSPF ingester 已实现(2026-07-11)**:shim 绑 `/dev/log`(syslog 接收器),FRR `log syslog` 推邻接变化 → 触发 `vtysh show ip ospf neighbor json` reconcile → `openconfig:/network-instances/.../ospfv2/areas/area/interfaces/interface/neighbors/neighbor[router-id]/state/adjacency-state`。实测启动快照 FULL 经 gNMI 暴露 + syslog 触发的 DEL(事件驱动)。覆盖指标 5。**8 类遥测指标全部有 ingester。**
     - **⚠️ 发现并修复严重死锁**:/dev/log 是 unix datagram(可靠投递),读得慢会阻塞 FRR 的 `syslog()` 拖垮 daemon。修复=解耦排空(只 drain + 非阻塞信号)+ 去抖 worker + 1MB rcvbuf。**原则:监控壳绝不能拖垮被监控者**(呼应 CoPP)。生产建议改「log file + inotify」彻底消除回压。
     - **修复已验证(独立 pe1/pe2 容器,非 clab)**:`clear ip ospf process` flap 期间 vtysh 稳定 21-25ms 秒回(修复前无限挂死);shim 经 syslog 触发完整周期 FULL→DEL→FULL。
5. **壳 v2**:
   - ✅ **Get + Capabilities 已实现(2026-07-11)**:Get 从 cache 查子树(用 `path.CompletePath` + `cache.Query`),三个 origin(openconfig/host/frr)均经 **gnmic 真实客户端**验证;Capabilities 返回版本/模型/编码清单。**读侧规范完整:Subscribe + Get + Capabilities。**
   - 待做:**Set(配置下发)**——L3=vtysh、L2=bridge/ip、mgmtd 已迁部分;写侧,风险高,从低风险子集起步。
6. **部署**:v0–v2 用方案 A(同容器)验证;定型后切方案 B(sidecar 共享 netns+pidns)。

---

## 11. 待定/需拍板

1. ~~指标 8「VPN」语义~~ **已定:基于 MPLS 的传统三层 VPN(RFC 4364)**。见 §5.4。→ 引出 VRF 一等维度 + 必须启用 ldpd。
2. **推送侧对齐**:gNMI dial-out(对齐 Arista,推荐)还是 dial-in / OTLP / 自定义 gRPC?
3. **路径模型**:纯 OpenConfig 标准(和 Arista 对齐,但 MPLS/VPN/容器指标字段 OC 覆盖不全需自定义 origin 补),
   还是一开始就用自定义 origin 私有路径先跑通、后贴 OC?**建议:核心指标贴 OC,覆盖不全处用自定义 origin 兜底,别追全量。**
4. ~~采集机制:轮询 vs 事件~~ **已定:事件驱动优先**(netlink 组播 / FPM / BMP / syslog / lldpd-watch),仅 gauge 类 SAMPLE。见 §3 / §12.5。

---

## 12. 数据模型(权威定义)

> 这是壳对外呈现、cache 内部组织的**唯一权威**。开发以此为准。
> 三条组织原则:
> 1. **三个 origin**:`openconfig`(标准、可复用)/ `host`(容器·系统,OC 无)/ `frr`(FRR 私有细节,OC 覆盖不全)。
> 2. **VRF = 顶层维度**:因指标 8 是 MPLS L3VPN,所有路由类数据挂在 `/network-instances/network-instance[name]` 下;
>    default VRF 本身也是一个 network-instance(`type=DEFAULT_INSTANCE`),每个 L3VPN VRF 是 `type=L3VRF`。
> 3. **编码 JSON_IETF**;每个 Notification 带时间戳;Subscribe 用 prefix + 相对 path。

### 12.1 origin 划分

| origin | 放什么 | 为什么 |
|--------|--------|--------|
| `openconfig`(或空) | 接口、VLAN、FDB、LLDP、BGP、OSPF、MPLS/LDP、路由表、CPU/内存(近似) | 标准、和 Arista 对齐、gnmic/OC dashboard 直接复用 |
| `host` | 容器 cgroup 指标(限额、throttle、swap)、容器身份 | OC 没有"容器"概念,别硬凑 |
| `frr` | L3VPN 的 RT/label 明细、LDP binding 明细、SPF 统计等 OC 建模不全的运维细节 | OC 的 l3vpn/mpls 覆盖弱,私有 origin 兜底,不追全量 |

### 12.2 cache 树骨架(list 的 key 已标注)

```
─ origin: openconfig ───────────────────────────────────────────────
/interfaces/interface[name]                         key=name
    /state/{admin-status, oper-status, mtu, type, ifindex}
    /state/counters/{in-octets,out-octets,in-pkts,out-pkts,
                     in-errors,out-errors,in-discards,out-discards,in-unicast-pkts,...}
    /ethernet/switched-vlan/config/{interface-mode(ACCESS|TRUNK), access-vlan, trunk-vlans[]}   # switchport
    /routed-vlan/...                                                                             # SVI
    /subinterfaces/subinterface[index]              key=index

/lldp/interfaces/interface[name]/neighbors/neighbor[id]   key=(name,id)
    /state/{chassis-id, port-id, system-name, system-description, management-address}

/network-instances/network-instance[name]           key=name   ← VRF 一等维度
    /config,/state/{name, type(DEFAULT_INSTANCE|L3VRF), router-id, route-distinguisher}
    /interfaces/interface[id]                        key=id     # 哪些口属于此 VRF
    /vlans/vlan[vlan-id]                             key=vlan-id  /state/{vlan-id,name,status}
    /fdb/mac-table/entries/entry[mac-address,vlan]   key=(mac-address,vlan)  /state/{interface,entry-type}
    /protocols/protocol[BGP,bgp]
        /bgp/neighbors/neighbor[neighbor-address]    key=neighbor-address
            /state/{session-state, peer-as, established-transitions, ...}
            /afi-safis/afi-safi[afi-safi-name]/state/prefixes/{received,sent,installed}
        /bgp/global/state/{as, router-id, total-prefixes}
    /protocols/protocol[OSPF,ospf]
        /ospfv2/global/state/{router-id}
        /ospfv2/areas/area[identifier]/interfaces/interface[id]/neighbors/neighbor[router-id]
            /state/{adjacency-state, priority, ...}
    /mpls/signaling-protocols/ldp/...                # LDP 邻居/会话(标准部分)
    /mpls/lsps/...
    /afts/ipv4-unicast/ipv4-entry[prefix]            key=prefix    # per-VRF RIB/FIB
        /state/{next-hop, pushed-mpls-label-stack}                 # push 栈=入口 PE 的 encap 标签(CSV)
    /afts/ipv6-unicast/ipv6-entry[prefix]

/interfaces/interface[name]/subinterfaces/subinterface[index]/ipv4/addresses/address[ip]
    /state/{ip, prefix-length}                        key=ip        # 接口地址(ip→node 反查)

/components/component[name]                          key=name      # CPU/内存的 OC 近似
    /cpu/utilization/state/{instant, avg}
    /state/memory/{available, utilized}

─ origin: host ─────────────────────────────────────────────────────
/container/cpu/state/{usage-usec, user-usec, system-usec, nr-throttled, throttled-usec}   # cgroup cpu.stat
/container/memory/state/{current, max, swap-current, limit}                               # cgroup memory.*
/container/state/{id, name}

─ origin: frr ──────────────────────────────────────────────────────
/l3vpn/vrf[name]                                     key=name
    /state/{rd, import-rt[], export-rt[], vpn-label, route-count}
/mpls/ldp/binding[fec,label]                         key=(fec,label)   /state/{peer, in-label, out-label}
/mpls/lfib/entry[label]                              key=label     # LFIB(全局标签交换表,path-trace 用)
    /state/{in-label, out-label, next-hop, interface}              # out 空+nh空+if=VRF ⇒ egress pop 进 VRF
/ospf/area[id]/state/spf/{count, last-duration-ms, last-run}
```

### 12.3 逐指标 → 路径 → 源 → 订阅模式(权威映射)

| # | 指标 | 主路径(origin) | 关键 leaf | 源 | 订阅模式 |
|---|------|----------------|----------|-----|---------|
| 1 | 容器 CPU/内存 | `host:/container/{cpu,memory}/state/*` | usage-usec, nr-throttled, current, max | cgroup `/sys/fs/cgroup`(cpu.stat/memory.*) | SAMPLE |
| 1' | (CPU/内存 OC 近似) | `oc:/components/component/{cpu,state/memory}` | utilization/instant, utilized | proc/cgroup | SAMPLE |
| 2 | 端口状态 | `oc:/interfaces/interface/state/{admin,oper}-status` | oper-status | netlink RTM_GETLINK 事件 | **ON_CHANGE** |
| 2 | 端口流量 | `oc:/interfaces/interface/state/counters/*` | in/out-octets, pkts, errors, discards | netlink stats64 | **SAMPLE**(gauge,物理上无事件)|
| 3 | VLAN | `oc:/network-instances/.../vlans/vlan`;`.../interface/ethernet/switched-vlan` | vlan-id, interface-mode, access/trunk | netlink AF_BRIDGE(bridge vlan) | ON_CHANGE |
| 3 | FDB(MAC 表)| `oc:/network-instances/.../fdb/mac-table/entries/entry` | mac-address, interface, vlan | netlink AF_BRIDGE(FDB)| ON_CHANGE |
| 4 | MPLS LFIB | `frr:/mpls/lfib/entry[label]/state/*`(已落地;规划曾为 `oc:.../afts/mpls/label-entry`,见 §15.4) | in/out-label, next-hop, interface | 内核 netlink **AF_MPLS** | SAMPLE 快照(可演进 ON_CHANGE)|
| 4 | MPLS 压栈标签 | `oc:.../afts/ipv4-unicast/ipv4-entry[prefix]/state/pushed-mpls-label-stack` | 入口 PE push 栈 | **FPM** nexthop 对象 `NHA_ENCAP` | **ON_CHANGE**(FPM)|
| 4 | LDP 邻居/绑定 | `oc:.../mpls/signaling-protocols/ldp` + `frr:/mpls/ldp/binding` | 邻居状态, in/out-label | ldpd(邻居=**syslog 事件**;绑定=低频 json 兜底)| 邻居 **ON_CHANGE**(syslog);绑定 SAMPLE |
| 5 | OSPF 邻居 | `oc:/network-instances/.../protocols/protocol[OSPF]/ospfv2/.../neighbors/neighbor` | adjacency-state | ospfd **syslog / SNMP-trap**(事件);json 仅初始快照 | **ON_CHANGE**(syslog)|
| 5 | OSPF 统计 | `frr:/ospf/area/state/spf/*` | spf count/duration | ospfd | SAMPLE |
| 6 | BGP 邻居 | `oc:/network-instances/.../protocols/protocol[BGP]/bgp/neighbors/neighbor/state` | session-state | bgpd **BMP**(peer up/down 推送)| **ON_CHANGE**(BMP)|
| 6 | BGP 前缀数 | `.../neighbor/afi-safis/afi-safi/state/prefixes/*` | received/sent/installed | bgpd(BMP 路由流推导,或 json SAMPLE 兜底)| 事件推导 / SAMPLE |
| 8 | L3VPN VRF | `oc:/network-instances/network-instance[VRF]/state/{type=L3VRF,route-distinguisher}` + `frr:/l3vpn/vrf` | rd, import/export-rt, vpn-label | bgpd **BMP**(VPNv4/v6 路由事件)+ json 补 RD/RT | **ON_CHANGE**(BMP)+ SAMPLE |
| 8 | L3VPN 路由 | `oc:/network-instances[VRF]/afts/ipv4-unicast/ipv4-entry` | prefix, label, next-hop | **FPM**(zebra 推 VRF FIB)/ netlink | **ON_CHANGE**(FPM)|
| 8 | PE-CE 邻居 | 同指标 5/6,但在对应 VRF 的 network-instance 下 | session/adjacency-state | bgpd **BMP** / ospfd **syslog**(per-VRF)| **ON_CHANGE** |
| 7 | LLDP 邻居 | `oc:/lldp/interfaces/interface/neighbors/neighbor/state` | chassis-id, port-id, system-name | lldpd **watch**(liblldpctl 订阅,事件)| **ON_CHANGE** |

> **说明**:指标 8(L3VPN)在模型里不是一棵独立子树,而是**"VRF 型 network-instance + 其下的 BGP/路由/MPLS + frr origin 的 RT/label 补充"**的组合——这正是 §5.4"VRF 一等维度"的落地。指标 2/5/6 一旦涉及非 default VRF,就落在对应 network-instance 下,自动带上 VRF 维度。

### 12.4 default VRF 与 L3VPN VRF 的关系(一句话)

- `network-instance[default]`(`type=DEFAULT_INSTANCE`):underlay——全局接口、OSPF/IS-IS、LDP、MPLS 传输标签、全局 BGP。
- `network-instance[<vpn>]`(`type=L3VRF`):每个客户 VPN——RD/RT、PE-CE 邻居、VPN 路由、VPN 标签。
- 两者在同一棵 `/network-instances/` 下并列,壳按 VRF 名索引;collector 订 `/network-instances/network-instance[name=*]/...` 即可一网打尽。

### 12.5 采集机制:事件驱动优先(权威)

**决策:能事件驱动的一律事件驱动;只有物理上无"变化事件"的 gauge 才 SAMPLE。** 壳内每类数据对应一个常驻 ingester:

| 数据 | 事件总线(push) | ingester 形态 | gNMI 模式 |
|------|----------------|--------------|-----------|
| 接口 up/down、路由、邻居(ARP)、FDB、MPLS-LFIB | **netlink 组播** | 常驻 netlink socket 订阅 `RTNLGRP_*` / `AF_BRIDGE` / `AF_MPLS` | ON_CHANGE |
| L3 路由 / FIB / VRF FIB / VPN 路由 | **FPM**(zebra push,已验证) | 壳内置 FPM 收 server(Netlink-over-TCP)| ON_CHANGE |
| BGP peer up/down + 路由(含 VPNv4/v6) | **BMP** | 壳内置 **BMP station**,`router bgp` 里配 `bmp targets` 指向它 | ON_CHANGE |
| OSPF 邻居状态 | **syslog**(或 SNMP-trap) | 壳解析 ospfd 日志的邻居状态迁移;或收 OSPF-MIB trap | ON_CHANGE |
| LLDP 邻居 | **lldpd watch** | 壳用 liblldpctl 订阅 lldpd(`lldpcli watch` 等价) | ON_CHANGE |
| **接口计数、CPU/内存、前缀数** | ❌ 无(gauge)| 定时读 netlink stats / cgroup / json | **SAMPLE** |

**要点:**
1. **这套事件总线恰好和既有 telemetry 设计同构**:FIB=FPM(已验证)、BGP=BMP(结构化推)、OSPF=syslog。壳只是把它们汇进一棵 gNMI cache。
2. **BMP 是把 BGP 从"轮询"变"事件"的关键**——FRR bgpd 原生支持,配 `bmp targets` 即把 peer 事件 + adj-rib 推给壳,零轮询拿到指标 6 和 8 的大头。
   **✅ 已实测(2026-07-10,my-frr VM,`router:v1` arm64)**:bgpd/zebra 均**主动 dial-out** 连收集器;建连推全量、之后只增量推;一次加路由,BMP(RouteMonitoring)+ FPM(RTM_NEWROUTE)秒级双双到达。模块:`-M bmp` / `-M dplane_fpm_nl`。
   - **eBGP IPv4 unicast**:Initiation/PeerUp/RouteMonitoring/StatsReport 全收到。
   - **MPLS L3VPN(指标 8 完整链路)**:PE1↔PE2 iBGP VPNv4 + `cust` VRF。**控制面**:`bmp monitor ipv4 vpn` → BMP RouteMonitoring 带 **RD=65000:2 / RT:65000:1 / Remote label=80**;**转发面**:FPM 推 VRF FIB `10.2.2.0/24 label 80`(`show mpls table` 亦见 `80 BGP cust`)。live 加 `10.9.9.0/24` → BMP + FPM 秒级同步。→ **指标 8 的"VPN 路由带 label 从 BMP 来、VRF FIB 从 FPM 来"证实。**(注:OSPF 核心链路需设 `ip ospf network point-to-point` 避开 40s DR 等待;LDP 传输标签与指标 8 正交,未起不影响遥测负载。)

**✅ 内核侧事件总线亦全部实测(2026-07-10,`router:v1` arm64)**:
   - **netlink 组播**:`ip monitor link route`、`bridge monitor` 实时捕获——link down/up(指标 2)、`bridge vlan add vid 100`(指标 3)、`fdb 00:11:22:33:44:55 static`(指标 3 FDB)、`ip route add`;`ip -f mpls monitor` 捕获 MPLS label 555 add/del(指标 4 LFIB,AF_MPLS 组播)。
   - **lldpd watch**:`apk add lldpd` + `lldpcli watch`,两容器经 docker bridge(需 `group_fwd_mask=0x4000` 放行 LLDP)互为邻居;l2 停 lldpd → l1 秒收 **"LLDP neighbor deleted"** 事件(指标 7,含 ChassisID/SysName/PortID/MgmtIP/Capability)。
   → **§12.3 中所有标 ON_CHANGE 的条目均有实测事件总线支撑;唯 gauge 类(计数/CPU/内存/前缀数)保持 SAMPLE。**
3. **`.vty` json / vtysh 不做高频轮询**,只做:① 启动时全量快照初始化 cache;② 事件总线覆盖不到的静态细节(SPF 统计、RD/RT)低频补充。
4. **gauge 类 SAMPLE 不是妥协**:计数器/CPU/内存本就没有"事件",Arista/Cisco 也 SAMPLE;做成 on-change 反而是错的。

## 13. 参考

- FRR / mgmtd:<https://github.com/FRRouting/frr> · <https://docs.frrouting.org/en/latest/mgmtd.html> · 迁移路线图 issue #5428 / #15615
- frr_exporter:<https://github.com/tynany/frr_exporter>
- goarista / ocprometheus:<https://github.com/aristanetworks/goarista> · <https://github.com/aristanetworks/goarista/tree/master/cmd/ocprometheus>
- telerista(IMC Trading):<https://pkg.go.dev/github.com/imc-trading/telerista>
- openconfig/gnmi(cache+subscribe):<https://github.com/openconfig/gnmi> · gnmic:<https://github.com/openconfig/gnmic> · gnmi-gateway:<https://github.com/openconfig/gnmi-gateway>
- lldpd:<https://github.com/lldpd/lldpd> · mstpd:<https://github.com/mstpd/mstpd>
- 事件总线:FRR **BMP**<https://docs.frrouting.org/en/latest/bmp.html> · FRR **FPM**(zebra `fpm`/`dplane_fpm_nl`)· netlink 组播(`RTNLGRP_*`)· lldpd watch(liblldpctl)
- 本地约束(记忆):FIB=FPM(已验证)、读=CoPP 限流。**注:本项目对 FRR 采用事件总线 BMP(BGP)/ FPM(路由)/ syslog(OSPF)——与 cEOS 场景(BGP=gNMI、OSPF=syslog 因无 gNMI 路径)分别选型,别混用。**

---

## 14. 端到端实测:5 节点拓扑 + 看板(2026-07-11)

把 8 类指标从"逐个验证"推进到**一个连贯拓扑上跑通、并接到 Grafana 看板**。所有步骤脚本化在 `lab/`,幂等可复现。

### 14.1 拓扑

```
ce1 ─── pe1 ─── p1 ─── pe2 ─── ce2          # 每台内嵌一个 shim(gNMI :9339)
        └────── VRF cust ──────┘
  OSPF area0 + LDP 核心(pe1-p1-pe2)          # 传输标签
  iBGP VPNv4 pe1↔pe2(update-source lo)       # VPN 标签 + RD/RT
  eBGP PE-CE(ce1 AS65101 / ce2 AS65102)      # 客户路由入 VRF
```

每台双平面:数据面 p2p(`l_*` docker 网络)+ 管理面 `campus-mgmt`(172.30.30.0/24),gnmic 从管理面订 `:9339`。mgmt IP:ce1 .101 / pe1 .111 / p1 .112 / pe2 .113 / ce2 .102。
实测:OSPF 全 FULL、LDP 全 OPERATIONAL、VRF FIB 里是真标签栈 `label 17/80`(LDP 传输 17 + BGP VPN 80)、ce1→ce2 环回源 ping 0% 丢包走 VPN。

### 14.2 遥测管道

```
5×shim(gNMI :9339,3 origin)
   → gnmic-frr 容器(172.30.30.20,prometheus output :9806)
   → Prometheus(:9090,job=gnmic-frr)
   → Grafana 看板 "frr-visible"(匿名 admin,http://localhost:3000/d/frr-visible)
```

- `gnmic-frr.yaml`:专用采集器,与 cEOS 的 gnmic 分开。复用 `oper-status`/`bgp-state` 的 str2num processor,新增 `ospf-state`(FULL=6..DOWN=0);字符串叶子(mac/next-hop/system-name/route-target)用 `strings-as-labels: true` 变标签、series=1,PromQL `count()` 数条数。
- 数值化后:oper-status→1/0,bgp session-state→6(Established),ospf adjacency→6(FULL);L3VPN `label` 本身数值(=80),`route-target` series 带 RT 标签。
- 看板 8 类面板 + `$node` 模板变量过滤:容器 CPU/内存、接口速率、端口状态时间线、OSPF/BGP/LLDP 邻居表、L3VPN 路由表(RD/prefix/label)、FIB/AFT 表、FDB MAC 表。

### 14.3 本轮修的 shim 真 bug(代码)

1. **`internal/ingest/netlink.go` 缺 FDB 初始快照**。`NeighSubscribe` 只推 ON_CHANGE 增量,shim 启动前已存在的 FDB 表项永远看不到。修:`snapshotFDB()` 用 `NeighList(0, AF_BRIDGE)` 拉一次全量,并加 `isUnicastMAC` 过滤掉 `33:33:*`/`01:00:5e:*` 组播噪声(同一过滤也用到实时 loop)。
2. **`internal/ingest/lldp.go` JSON 数组形态没解析**。lldpd 1.0.x `-f json` 输出 `lldp.interface` 是**数组、每元素是单键对象** `[{"eth0":{...}}]`,而旧代码只认 `[{"name":"eth0",...}]`,导致全被跳过。修:数组分支加"单键对象即接口名"的处理。
3. **`internal/gnmiserver/server.go` Subscribe 强制要 prefix.target**。openconfig `subscribe.Server` 对没带 target 的请求直接 `InvalidArgument: request must contain a prefix`,而通用采集器(gnmic/Telegraf)默认不发 target。修:加 `targetDefaultingStream` 包装流,客户端没给时补本机 cache target——单 target 的 gNMI 盒子本就不该要求客户端知道内部 target 名。

### 14.4 踩坑纠正(环境/配置层)

- **⚠️ 无键 list 不当通配(最影响使用)**。这个 openconfig cache 对**无键 list 元素按字面匹配、不当 wildcard**(gNMI 规范里缺 key = all)。后果:`.../interface/state/oper-status` 这种精确 leaf 路径 **Get/Subscribe 都返回空**,只有子树订阅(`/interfaces`)能匹配。**采集路径必须显式写 `[key=*]`**——`gnmic-frr.yaml` 里每个 list 都通配了。(可选后续:在 shim 侧把无键元素重写为通配,让它符合规范。)
- **docker bridge 默认丢 LLDP 组播**(`01:80:c2:00:00:0e`)。要给每个承载链路的网桥设 `group_fwd_mask=0x4000`,lldpd 才能互相学到邻居。
- **FPM/BMP 是 FRR 可加载模块,要 `-M` 载入**。`router:v1` 的 daemons 文件里 zebra 要 `-M dplane_fpm_nl`、bgpd 要 `-M bmp`,否则 `fpm address` / `bmp targets` 命令不认。
- **eBGP 默认要策略**。FRR `bgp ebgp-requires-policy` 默认开,PE-CE 前缀被 `(Policy)` 拦;lab 里 CE 和 PE-vrf 实例都加 `no bgp ebgp-requires-policy`。
- **LDP 集成配置有加载时序坑**。整合 `frr.conf` 里的 `mpls ldp` 块可能没被 ldpd 及时吃进去;改成**启动后用 `vtysh -c` 下发** LDP 更可靠(`build-topo.sh` 就这么做)。没有 LDP 传输标签,VPNv4 远端 PE 下一跳解析不出 LSP,VRF FIB 装不上路由。
- **p2p docker 网络别把 `.1` 给容器**——docker 把子网 `.1` 占作网关。lab 用 `.254` 做网关腾出 `.1/.2`。
- **VRF enslave 保留 IPv4**。这版内核 `ip link set <if> master <vrf>` 不会 flush 地址,重复 `ip addr add` 会报"已分配",要容错。

### 14.5 复现(虚机 my-frr 内)

```bash
bash lab/build-topo.sh      # 建 5 节点拓扑(幂等,含 teardown)
bash lab/check-topo.sh      # 验 OSPF/LDP/BGP/L3VPN 收敛
bash lab/deploy-shim.sh     # 编译+嵌入部署 shim、装 lldpd、建 bridge/FDB、配 FPM/BMP/OSPF-syslog
bash lab/setup-telemetry.sh # 起 gnmic-frr、加 prometheus job、装看板
# 打开 http://localhost:3000/d/frr-visible
```

> **注(2026-07-11 后续)**:拓扑已从 5 节点扩到 **8 节点**(2×PE + 2×P + 4×CE),数据面改为 **veth 点对点直连**(接口名确定,如 `pe1-p1`/`pe1-ce1`),管理面改为**专属 `frr-mgmt`(172.31.0.0/24)**,与共享网络分离。协议配置从 `build-topo.sh` 拆出到独立的 `config-l3vpn.sh`(先做连接、再配协议)。所有 lab 脚本已相应重写。

## 15. 路径追踪(path tracing):Linux MPLS 的坑与"控制面重建"方案(2026-07-11)

补齐"统一诊断"里的**路径追踪**能力。先厘清:可观测性第三支柱的**分布式 trace(OpenTelemetry/span)是软件/微服务概念,不套用于路由器数据面**——一台路由器转发包不是"带 trace ID 的请求流经一串服务"。网络里的"trace"是另外的东西:**路径追踪(traceroute)** 和**流追踪(sFlow/IPFIX)**。这里解决路径追踪。

### 15.1 关键发现:Linux MPLS 核心对 IP traceroute 隐身(已实测定性)

在 8 节点 L3VPN 上跑 `traceroute ce1 -> ce4`,结果核心 P 节点全是 `*`:

```
1  10.0.21.1 (pe1)     ← PE-CE 是纯 IP,可见
2  *        (p1)       ← 核心 P 节点隐身
3  *        (p2)
4  10.255.1.4 (ce4)
```

**根因(内核计数器级证据,非黑盒臆测)**:Linux 的 MPLS 转发面**对 TTL 过期的标签包不生成 ICMP Time Exceeded**。验证方法——观察同一台 LSR(p1)的 `IcmpOutTimeExcds` 计数器:

| 触发 | `IcmpOutTimeExcds` delta |
|---|---|
| 纯 IP TTL 过期(经 p1)| **+2**(生成了) |
| MPLS 标签 TTL 过期(经 p1)| **0**(根本没生成) |

计数器在 `icmp_send()` **生成那一刻**就 +1,delta=0 排除了"生成了但被内部丢弃",证明是**根本没生成**——`mpls_forward()` 在 TTL 归零时直接 drop。实测内核 **Ubuntu 6.17.0-40**(2026 很新的版本)依然如此。**没有 sysctl 能开启**(是代码路径问题)。

**踩坑纠错**:一开始用"p1 有到源的直连路由却不回 ICMP"来"证明"没生成——**这个推理是错的**。因为按 RFC 标准,LSR 生成的 ICMP 应该**沿 LSP 顺向压标签送到出口 LER、再由出口路由回源**(ICMP 隧道化,配合 RFC 4950 标签扩展),而不是走直连 IP 直接回源;所以 p1 有没有直连回程路由**不相关**。真正的证据是上面的内核计数器。

**结论的边界**:ICMP 隧道化是**标准/商用设备(Cisco/Juniper)**的正确行为;而**主线 Linux(至今 6.17)的 MPLS 数据面没有实现它**——过期标签包静默丢弃,故 Linux MPLS 核心天生对 traceroute 隐身。

### 15.2 方案:控制面路径重建(control-plane path walk)

既然数据面 ICMP 探不了核心,就**顺着转发表逐跳走**:每个节点查它对当前前缀/标签的转发决定(出接口、下一跳、标签 push/swap/pop),接力到下一跳。**在 Linux 上 FIB 就是数据面(netlink 编程进内核),这是权威的**,而且比 traceroute 信息更全(每跳标签栈都有)、免疫 ICMP 缺陷。

工具 **`lab/pathtrace.sh <起点> <目的IP>`**:用 vtysh JSON + jq,带一个标签栈状态机(push / swap / PHP-pop / 出口弹标签进 VRF)。`ce1 -> ce4` 输出:

```
ce1   IP/default            via ce1-pe1  -> 10.0.21.1
pe1   IP/cust   push[18,80]  via pe1-p1   -> 10.0.12.2      # 压 传输18 + VPN80
p1    MPLS      swap 18->16   via p1-p2    -> 10.0.13.2   stack[16 80]
p2    MPLS      pop 16 (PHP)  via p2-pe2   -> 10.0.14.2   stack[80]   # PHP 弹传输标签
pe2   MPLS      pop 80   -> VRF cust                                   # 弹 VPN 标签进 VRF
pe2   IP/cust              via pe2-ce4  -> 10.0.24.2
ce4   [dest]  destination reached
path: ce1 -> pe1 -> p1 -> p2 -> pe2 -> ce4
```

核心 p1/p2 全现形,每跳标签操作精确。双向通用(反向 ce4→ce1 标签不同,如 pe2 压 `[19,80]`),证明是实时读每节点 LFIB。

### 15.3 落进"统一诊断"的形态

- **分工**:CE↔PE 纯 IP 边缘用 IP traceroute(可见);**MPLS 核心用控制面重建**。合起来是完整端到端 trace。
- **gNOI Traceroute 的注意点**:若给 shim 实现 gNOI `System.Traceroute`(内部跑 `traceroute`),在 FRR 上核心会如实返回 `*`——这是 Linux MPLS 固有行为,不是 bug,须在文档写明。
- **正版演进**:`pathtrace.sh` 现在是 `docker exec vtysh` 登设备读表;正版应**读 shim 的 gNMI**——AFT FIB(`openconfig:/network-instances/.../afts/.../next-hop`)与 MPLS 标签数据 shim 已在 cache 里,path-trace 即"节点接节点查 gNMI",与 cEOS 同接口,真正统一。这才是 Linux/FRR MPLS 上路径追踪的正确落地。**已落地,见 §15.4。**

### 15.4 trace 升级:从 vtysh 到 gNMI(2026-07-11,已实测)

§15.3 的"正版演进"已实现。分两步:先给 shim 补齐 MPLS 转发面的导出(前提),再把 trace 改成纯 gNMI。

**前提发现**:原 `fpm.go` 只导出 `afts/ipv4-unicast/ipv4-entry[prefix]/state/next-hop`——**没有压栈标签,也没有 LFIB(标签交换表)**。而 path-trace 恰恰走标签栈(pe1 `push 18/80`、p1 `swap 18→16`、p2 `PHP pop`、pe2 `pop 80→VRF`)。所以 gNMI 版 trace 建不起来,**必须先升级 shim**。好消息:数据本就到了 shim 门口(FPM 送 nexthop 对象 + 内核有 AF_MPLS 表),只是没解析。

**Step A — shim 新增三项导出(代码)**:

| 导出 | 路径 | 源 | 说明 |
|------|------|-----|------|
| IP 路由压栈标签 | `openconfig:.../afts/ipv4-unicast/ipv4-entry[prefix]/state/pushed-mpls-label-stack`(CSV,如 `18,80`) | FPM:`NHA_ENCAP`/`RTA_ENCAP`(MPLS)| 入口 PE 的 push 栈 |
| MPLS LFIB(标签交换表)| `frr:/mpls/lfib/entry[label]/state/{in-label,out-label,next-hop,interface}` | 内核 netlink **AF_MPLS** 路由(`netlink` 库原生解析)| swap/PHP-pop/egress-pop 三态 |
| 接口 IPv4 地址 | `openconfig:/interfaces/interface[name]/subinterfaces/subinterface[index=0]/ipv4/addresses/address[ip]/state/{ip,prefix-length}` | netlink `AddrList` | 让客户端把下一跳 IP 反查到节点,免登设备 |

> **关键 bug(已修)**:内核对嵌套属性置 `NLA_F_NESTED (0x8000)` 标志位,故 `NHA_ENCAP(8)` 实际到达是 `0x8008`,原 `forEachAttr` 精确比较 `8` 永远不匹配,压栈标签一直为空。修法:`forEachAttr` 统一把类型与 `0x3fff` 掩码,剥掉 `NLA_F_NESTED`/`NLA_F_NET_BYTEORDER`。
>
> **LFIB 三态映射**(直接对应 `ip -f mpls route`):`out-label` 非空=**swap**;`out-label` 空 + `next-hop` 非空=**PHP pop**(隐式空标签);`out-label` 空 + `next-hop` 空 + `interface`=VRF 名=**egress pop 进 VRF**。
>
> **模型偏差(诚实记录)**:§12.2/12.3 曾把 LFIB 规划到 `oc:.../afts/mpls/label-entry`、ON_CHANGE。实际落地为 `frr:/mpls/lfib/entry`(LFIB 是全局表、不是 per-VRF,放 `frr` origin 更贴切)、SAMPLE 快照(LFIB 变动少,快照够用;后续可换 `RTNLGRP_MPLS_ROUTE` 订阅改 ON_CHANGE)。

**Step B — `lab/pathtrace-gnmi.sh`(纯 gNMI,不登设备)**:同一套标签栈状态机,但每一跳都是对该节点**管理地址**发 `gnmic get`——与 trace cEOS 完全同接口。两个设计点:

- **VRF 搜索交给服务端**:用 `network-instance[name=*]` 通配一次查所有 VRF,命中哪个 VRF 由返回的 path key 自带,tracer **完全不需要跟踪 VRF 上下文**。
- **LPM 放客户端**:gNMI key 是精确匹配、不做最长前缀匹配,故脚本把返回的所有 `ipv4-entry` 在本地做 LPM。

**实测结果**(8 节点 veth 拓扑,ce1→ce4,耗时 0.26s):

```
ce1   IP/default            -> 10.0.21.1  (10.255.1.4/32)
pe1   IP/cust   push[18,80]  -> 10.0.12.2  (10.255.1.4/32)
p1    MPLS      swap 18->16   -> 10.0.13.2  (p1-p2)   stack[16 80]
p2    MPLS      pop 16   (PHP) -> 10.0.14.2  (p2-pe2)   stack[80]
pe2   MPLS      pop 80   -> VRF cust
pe2   IP/cust              -> 10.0.24.2  (10.255.1.4/32)
ce4   [dest]       destination reached
path:  ce1 -> pe1 -> p1 -> p2 -> pe2 -> ce4
```

与 vtysh 版 `pathtrace.sh` 逐跳一致,双向对称(反向 ce4→ce1 用标签 19→16)。`pathtrace.sh`(登设备)保留为离线兜底,`pathtrace-gnmi.sh` 是统一的、免登设备的正版。

### 15.5 path trace 上看板(2026-07-11,已实测)

把 on-demand 的 trace 变成**持续指标**,进 Prometheus + Grafana。三支柱里 trace 支柱的"数据面 path trace"这半落地。

**`cmd/pathtrace-exporter`(Go,直连 gNMI,不依赖 gnmic/bash)**:周期(15s)对配置的**流**跑一遍 §15.4 的 gNMI 走法(地址建 ip→node、通配 VRF 的 AFT 做 LPM、走 LFIB),把结果吐成 Prometheus 文本、自服务 `/metrics`。所以能塞进任意最小容器(alpine 无 bash 也行)。
- 配置(env):`INVENTORY`(node=mgmtIP,…)、`FLOWS`(name:startNode>dstIP,…)、`INTERVAL`、`LISTEN`。
- 指标:`frr_pathtrace_reachable{flow,src,dst}`、`frr_pathtrace_hops{...}`、`frr_pathtrace_duration_seconds{...}`、`frr_pathtrace_hop_info{flow,seq,node,kind,nexthop,labels,detail}=1`(每跳一条 series,`kind`∈ ip / ip-push / mpls-swap / mpls-pop-php / mpls-pop-vrf / dest / drop)。

**部署**(`setup-telemetry.sh` 已并入,幂等):exporter 容器**双挂**——`frr-mgmt`(172.31.0.31,够到 8 个 shim :9339)+ `campus-mgmt`(172.30.30.21,让 Prometheus 抓 :9808),与 `gnmic-frr` 同套路。Prometheus 加 `pathtrace-exporter` job;看板加一行 **Trace**:`Flow reachable`(1=OK 背景红绿)、`Hops per flow`、`Current path — per hop`(表,按 flow+seq 排序,逐跳 node/kind/next-hop/labels/detail)。

**实测**:3 条流(ce1→ce4 / ce4→ce1 / ce2→ce3)均 reachable=1、hops=7,`hop_info` 逐跳与 §15.4 一致;Prometheus `sum(frr_pathtrace_reachable)=3`。看板 `FRR-visible`(Mac 上 http://localhost:3000/d/frr-visible)。这样一条路径断在哪一跳/哪个 VRF、标签栈怎么变,看板上直接可见,还能告警(reachable→0)。

> 下一步(紧随):**convergence trace**——控制面收敛事件的跨进程因果时间线,补齐 trace 支柱的另一半。**已落地,见 §15.6。**

### 15.6 convergence trace:控制面收敛的跨进程因果时间线(2026-07-11,已实测)

路由器版"三支柱"的 trace 支柱有两半:§15.1–15.5 是**数据面 path trace**(包沿设备逐跳);这一节是**控制面 convergence trace**——一次拓扑事件(链路/邻接变化)在**一台设备内多个进程**间引发的因果时间线,用 span 表示。对标微服务分布式追踪:"服务"=zebra/ospfd/bgpd 等进程,"请求"=一次收敛,"span"=每个进程/阶段的处理。

**为什么 shim 能做**:它已经把该设备所有内部总线接上、都带时间戳(netlink / OSPF syslog / FPM / BMP)。做 trace 只差把这些事件按因果串起来。

**实现(`internal/correlate`)**:各 ingester 在产生事件时,除写 cache 外再 `cor.Emit(bus, kind, key, detail, root)`。correlator 单 goroutine 串行折叠:
- **root 事件**(link-down/up、adj-down/up = 拓扑触发)开一条 trace 窗口;**follow 事件**(route add/del、vpn withdraw/announce)只在窗口内追加成 span。**无 root 的 follow 直接丢弃——这天然过滤了启动时的全量 sync**(全是 follow)。
- 时间窗聚类:相邻事件间隔 > window(3s)或静默 > idle(3s)则 flush 整条 trace。
- `Emit` **nil-safe + 非阻塞**(缓冲满即丢并记一行),绝不拖累 ingester(牢记 §12.5 的 /dev/log 死锁教训:监控不能拖垮被监控者)。
- 输出:`[trace] {json}` 日志 + HTTP `:9340/traces`(最近 50 条 JSON)。`lab/traceview.sh <mgmt-ip> [all|last|<id>]` 渲染成瀑布图。

**实测(pe1-p1 flap)**:

```
== convergence trace @ pe1  #1 ==   root: netlink/link-down pe1-p1   span=308ms
   +    0ms  netlink/link-down   pe1-p1        oper=DOWN
   +    8ms  fpm/route-del       10.0.12.0/30  vrf=default     ← zebra 立即撤传输路由
   +    8ms  fpm/route-del       10.255.0.11/32 (loopback…)
   +   69ms  fpm/route-del       10.255.1.4/32 vrf=cust        ← VPN 前缀(ce4)撤销
   +   69ms  fpm/route-del       10.255.1.3/32 vrf=cust
   +  308ms  ospf/adj-down       10.255.0.11   if=pe1-p1       ← OSPF 邻居 down(最慢)
== convergence trace @ pe1  #3 ==   root: ospf/adj-full 10.255.0.11   (link-up 后)
   +    0ms  ospf/adj-full       10.255.0.11   INIT->FULL      ← 恢复后约 10s 才 FULL
```

**关键洞察**:同一事件里 **zebra 的 nexthop-tracking(8ms)比 OSPF 邻居检测(308ms)快两个数量级**;恢复时收敛"卡在"OSPF 重新建邻(~10s)。这正是收敛分析要回答的"用了多久、卡在哪个进程/阶段"。

**诚实边界**:
- ~~**单设备**~~。已扩展为跨设备聚合,见 §15.7。
- **因果是时间窗重建、非传播的 trace-id**(路由协议无 traceparent 传播),突发叠加时关联会有歧义——§15.4 已述。
- BMP withdraw 未进窗口:iBGP 到对端走 loopback,邻居 down 要等 hold timer(>3s 窗口),符合预期。
- veth 恢复时 operState 会抖动(DOWN→UP),root 标签偶尔不精确,如实保留。

**上看板/正式化(紧随)**:convergence trace 是事件流,天然适合 **Loki**(把 `[trace]` JSON 喂 Loki)或 **Tempo**(correlator 输出 OTLP span,Grafana 瀑布图)。原型阶段先 `traceview.sh` 命令行 + HTTP 端点。

### 15.7 跨设备聚合:端到端分布式收敛 trace(2026-07-11,已实测)

§15.6 是单设备。这一节把各节点的 per-device trace **拼成一条跨设备的分布式 trace**——一次拓扑事件在它波及的每台路由器上的时间线,合并成一条。

**放宽 correlator(前提)**:远端设备(离断链点远)接口没断、邻居没掉,只有**纯 follow 事件**(FIB 因远端拓扑变化而更新),会被 §15.6 的"无 root 丢弃"过滤掉——但端到端 trace 恰恰要看到远端何时收敛。于是放宽:**warmup(15s,过滤启动全量 sync)之后,一簇 follow 事件**(远端 FIB churn)**自成一条 churn trace**;零星单条(flush 时 span<3)丢弃,避免噪音。

**聚合器(`cmd/trace-aggregator`,Go)**:周期拉所有节点 `:9340/traces`,按 **start 时间聚类**(window 默认 1.5s)成分布式 trace,把各节点的 span 合并、按绝对时间排序、标注来源节点;并把**链路端点规范化**(`pe1-p1` 与 `p1-pe1` → `p1--pe1`)作为关联佐证。暴露 `:9341/dtraces`;`lab/dtraceview.sh <agg-ip>` 渲染跨设备瀑布。部署同 pathtrace-exporter(frr-mgmt 172.31.0.32,`setup-telemetry` 可并入)。

**关联信号(实测极强)**:一次 `pe1-p1` 断链,两端 `link-down`(pe1-p1 / p1-pe1)start 时间**对齐到 <1ms**,端点规范化互指——时间 + 链路两个独立信号都指向同一事件。

**实测(一次 pe1-p1 断链的端到端 dtrace)**:

```
== distributed convergence trace #1 ==   link=p1--pe1  span=308ms  nodes=[ce1,ce2,ce3,ce4,p1,pe1,pe2]
   +   0ms  pe1  netlink/link-down  pe1-p1          ← 断链点
   +   1ms  p1   netlink/link-down  p1-pe1          ← 另一端(亚毫秒对齐)
   + 12ms  p1/pe2 fpm/route-del     10.255.0.1/32…  ← zebra 撤传输路由
   + 73ms  pe1/pe2 fpm/route-del    10.255.1.x/32   ← VPN 前缀(CE loopback)
   +124ms  ce1..ce4 fpm/route-del   10.255.1.x/32   ← eBGP 撤销传到 CE 边缘
   +308ms  pe1/p1 ospf/adj-down     (双向)          ← OSPF 邻居检测(最慢)
```

**洞察**:一次断链在 **~308ms 内涟漪扩散到 7 个节点**,传播波清晰:zebra FIB(12ms)→ 跨设备 BGP 逐层外扩到 CE(73→125ms)→ OSPF 邻居检测(308ms)。这是**整网端到端收敛的因果时间线**,回答"事件多快传到边缘、卡在哪一层"。

**诚实边界**:
- **因果靠时间聚类重建**(window 1.5s),非传播的 trace-id;窗口大小是"聚全 vs 误并不同事件"的权衡。
- **依赖跨设备时钟同步**:本 lab 所有容器同宿主机同内核时钟,故 start 对齐到亚毫秒;**真实多设备网络需 NTP/PTP**,聚类窗口要相应放大。这是把此法用于生产的关键前提。
- churn 阈值(span≥3)会滤掉变化极少的过路节点(实测 p2 只变 2 条→未计入),是"抓收敛 vs 抑噪音"的取舍。
- **正式化下一步**:correlator 出 **OTLP span** → **Tempo**,则 Grafana 直接渲染这张跨设备瀑布(真正的分布式追踪 UI),并可与 metrics(pathtrace-exporter)、logs(syslog/Loki)在同一看板联动——路由器版三支柱闭环。
