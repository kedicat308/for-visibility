# frr-visible

**EN** — A **gNMI shim** that wraps an FRR container: event-driven ingesters feed FRR/kernel state into an OpenConfig cache, exposed over gNMI (Subscribe / Get / Capabilities). It makes an FRR box *look like* a gNMI-speaking router-switch without modifying FRR. Full design in [`design.md`](design.md).

**中** — 给 FRR 容器体外套一层 **gNMI 壳**:事件驱动 ingester 把 FRR/内核状态灌进一棵 OpenConfig cache,对外用 gNMI(Subscribe / Get / Capabilities)暴露。不改 FRR,让它"看起来像"会说 gNMI 的路由交换机。完整设计见 [`design.md`](design.md)。

## Architecture / 架构

```
FRR daemons ─ BMP (BGP/L3VPN) ──┐
            ─ FPM (routes/FIB) ─┤
kernel ─ netlink (if/vlan/fdb) ─┤→ ingesters → internal/state (openconfig/gnmi cache, "cache-centric")
       ─ cgroup (cpu/mem) ──────┤            → internal/gnmiserver (Subscribe + Get + Capabilities)
lldpd ─ lldpcli ────────────────┤            → gNMI client (gnmic, Telegraf, CloudVision, …)
ospfd ─ syslog ─────────────────┘
```

**EN** — Event-driven where the kernel/protocol pushes (netlink multicast, FPM, BMP, syslog, lldpd); SAMPLE only for true gauges (counters, CPU/mem) — same as commercial NOS. Three origins share one cache: `openconfig` (standard), `host` (container, not in OC), `frr` (L3VPN RD/RT/label OC covers poorly).

**中** — 凡是内核/协议主动推的就事件驱动(netlink 组播、FPM、BMP、syslog、lldpd);只有真正的 gauge(计数、CPU/内存)才 SAMPLE——和商用 NOS 一致。三个 origin 共用一棵 cache:`openconfig`(标准)、`host`(容器,OC 没有)、`frr`(L3VPN 的 RD/RT/label,OC 覆盖弱)。

## Metric coverage / 指标覆盖 (8/8)

| # | Metric / 指标 | Ingester | Path (origin) |
|---|------|----------|------|
| 1 | Container CPU/mem / 容器 CPU·内存 | cgroup | `host:/container/{cpu,memory}/state/*` |
| 2 | Port status/traffic / 端口状态·流量 | netlink | `openconfig:/interfaces/interface/state{,/counters}` |
| 3 | VLAN / FDB | netlink | `openconfig:/network-instances/.../fdb/mac-table` |
| 4 | MPLS FIB | fpm | `openconfig:/network-instances/.../afts` |
| 5 | OSPF neighbors / OSPF 邻居 | ospf (syslog) | `openconfig:/network-instances/.../ospfv2/.../neighbor` |
| 6 | BGP neighbors / BGP 邻居 | bmp | `openconfig:/network-instances/.../bgp/neighbors/neighbor` |
| 7 | LLDP | lldp | `openconfig:/lldp/interfaces/.../neighbor` |
| 8 | MPLS L3VPN | bmp + fpm | control: `frr:/bgp-rib/.../route[rd][prefix]` · forwarding: `openconfig:.../afts` |

**EN** — For one VPN route, control plane (BMP: RD/RT/label) and forwarding plane (FPM: VRF FIB) align in the same cache — the core value.
**中** — 同一条 VPN 路由,控制面(BMP:RD/RT/label)和转发面(FPM:VRF FIB)在同一棵 cache 对齐——核心价值。

## gNMI RPC coverage / gNMI RPC 覆盖

- ✅ **Subscribe** — streaming telemetry (STREAM/ONCE + ON_CHANGE/SAMPLE) / 流式遥测
- ✅ **Get** — one-shot snapshot, verified on all 3 origins with gnmic / 一次性快照,三 origin 均经 gnmic 实测
- ✅ **Capabilities** — model/encoding discovery / 模型·编码发现
- ⬜ **Set** — config push; the only remaining piece (write side, higher risk) / 配置下发,唯一剩余(写侧,风险高)

## Layout / 目录

- `cmd/frr-visible` — main program (cache + gNMI server + all ingesters) / 主程序
- `cmd/subtest` — tiny gNMI Subscribe test client (`-once`/STREAM, `-origin`, `-path`) / 验证客户端
- `internal/state` — OpenConfig cache wrapper / cache 封装
- `internal/gnmiserver` — gNMI server: Subscribe + Get + Capabilities / gNMI 服务端
- `internal/ingest/fpm.go` — routes / VRF FIB + nexthop-group parsing / 转发面
- `internal/ingest/bmp.go` — BGP peer state + VPNv4 routes (RD/RT/label) / 控制面
- `internal/ingest/netlink.go` — interface status (ON_CHANGE) / counters (SAMPLE) / FDB
- `internal/ingest/lldp.go` — lldpcli watch trigger + json reconcile + 15s fallback
- `internal/ingest/cgroup.go` — container CPU/memory (host origin, SAMPLE)
- `internal/ingest/ospf.go` — OSPF neighbors via syslog trigger + vtysh reconcile
- `internal/ingest/vrf.go` — VRF table-id → name (netlink)
- `lab/` — reproducible 8-node MPLS L3VPN lab **+ end-to-end Grafana dashboard** (8/8 metrics) / 8 节点可复现实验 + 端到端看板
  - `build-topo.sh` — 8-node topology (2×PE, 2×P, 4×CE), **veth point-to-point** data plane + dedicated `frr-mgmt` net (connectivity only)
  - `config-l3vpn.sh` — OSPF+LDP core, iBGP VPNv4, VRF cust, PE-CE eBGP (protocols, run after build); `check-topo.sh` — convergence check
  - `deploy-shim.sh` — compile + embed the shim into all 8, wire FPM/BMP/OSPF-syslog, install lldpd, bridge/FDB
  - `gnmic-frr.yaml` — gnmic collector (shim gNMI → Prometheus :9806); `frr-visible-dashboard.json` — Grafana dashboard; `setup-telemetry.sh` — wiring (incl. pathtrace-exporter)
  - `cmd/pathtrace-exporter` — Go exporter that runs the gNMI path trace for configured flows and serves it as Prometheus metrics (`frr_pathtrace_*`), shown on the dashboard's **Trace** row — see `design.md` §15.5
  - `traceview.sh` — renders the shim's **convergence trace** (control-plane causal timeline of a link/adjacency event across netlink/OSPF/FPM/BMP, served at `:9340/traces`) as a waterfall — see `design.md` §15.6
  - `cmd/trace-aggregator` + `dtraceview.sh` — stitch every node's convergence trace into one **cross-device distributed trace** (a topology event's ripple across the whole network, correlated by time + normalized link endpoints), served at `:9341/dtraces` — see `design.md` §15.7
  - `pathtrace.sh` — control-plane path trace via `vtysh` (walks FIB/LFIB hop-by-hop with the label stack; sees the MPLS core that IP traceroute can't — see `design.md` §15)
  - `pathtrace-gnmi.sh` — the same trace sourced **entirely from the shim's gNMI** (no device login; reads AFT push-labels + `frr:/mpls/lfib` + interface addresses) — like tracing across cEOS; see `design.md` §15.4

## Build / Run / 构建·运行

Go 1.24+. Deploy either **embedded** (shim inside the FRR container — CPU/mem = FRR container, lldpcli/vtysh reachable) or **sidecar** (`--network container:<frr>`, shares netns).
构建需 Go 1.24+。部署可选**嵌入式**(shim 跑在 FRR 容器内——CPU/内存即 FRR 容器,lldpcli/vtysh 可达)或 **sidecar**(`--network container:<frr>`,共享 netns)。

```bash
CGO_ENABLED=0 go build -o /tmp/frr-visible ./cmd/frr-visible
CGO_ENABLED=0 go build -o /tmp/subtest     ./cmd/subtest

# embedded: run the shim inside the FRR container / 嵌入式:shim 跑进 FRR 容器
docker cp /tmp/frr-visible pe1:/shim
docker exec -d pe1 sh -c "/shim -gnmi :9339 -fpm 127.0.0.1:2620 -bmp 127.0.0.1:5000 -target frr"

# point FRR's FPM/BMP at the shim, enable OSPF syslog / 把 FRR 的 FPM/BMP 指向 shim,开 OSPF syslog
docker exec pe1 vtysh -c "conf t" -c "fpm address 127.0.0.1 port 2620" \
  -c "router bgp 65000" -c "bmp targets T1" -c "bmp connect 127.0.0.1 port 5000 min-retry 1000 max-retry 5000"
docker exec pe1 vtysh -c "conf t" -c "log syslog informational" -c "router ospf" -c "log-adjacency-changes detail"

# query with the real gnmic client / 用真实 gnmic 客户端查询
gnmic -a 172.30.0.11:9339 --insecure capabilities
gnmic -a 172.30.0.11:9339 --insecure get --path "openconfig:/interfaces/interface[name=eth0]/state/oper-status"
gnmic -a 172.30.0.11:9339 --insecure get --path "frr:/bgp-rib/afi-safis/afi-safi[name=l3vpn-ipv4-unicast]/routes"
```

### End-to-end lab + dashboard / 端到端实验与看板

Inside a host with FRR containers, build the 8-node backbone, deploy the shim, and wire the dashboard — all idempotent:
在装有 FRR 容器的宿主上,一次建好 8 节点骨干、部署 shim、接通看板(全部幂等):

```bash
bash lab/build-topo.sh      # 8-node topology (2xPE 2xP 4xCE), veth p2p data plane + frr-mgmt net (connectivity)
bash lab/config-l3vpn.sh    # OSPF+LDP core, iBGP VPNv4, VRF cust, PE-CE eBGP (protocols)
bash lab/check-topo.sh      # verify OSPF FULL / LDP OPERATIONAL / VPNv4 / L3VPN forwarding
bash lab/deploy-shim.sh     # build + embed shim in all 8, install lldpd, bridge/FDB, wire FPM/BMP/OSPF-syslog
bash lab/setup-telemetry.sh # gnmic-frr + pathtrace-exporter -> Prometheus -> Grafana
# open http://localhost:3000/d/frr-visible

bash lab/pathtrace.sh ce1 10.255.1.4        # control-plane path trace ce1 -> ce4 (via vtysh)
bash lab/pathtrace-gnmi.sh ce1 10.255.1.4   # same trace, sourced entirely from the shim's gNMI (no device login)
```

All 8 metric categories carry real data on a coherent topology; the dashboard has a `$node` filter and panels for CPU/mem, interface rate + state timeline, OSPF/BGP/LLDP neighbor tables, and L3VPN / FIB / FDB tables. See `design.md` §14 for the full write-up (incl. the 3 shim bug-fixes and the environment gotchas).
8 类指标在一个连贯拓扑上都有真实数据;看板含 `$node` 过滤和全部 8 类面板。完整记录见 `design.md` §14(含 3 个 shim bug 修复与环境踩坑)。

## Gotchas / 踩坑记录

- **⚠️ /dev/log back-pressure deadlock / 回压死锁 (important)** — Binding `/dev/log` (unix datagram, *reliable* delivery) as a syslog sink: if the reader is slow (e.g. inline `fork vtysh` per message), the receive buffer fills and FRR's `syslog()` **blocks**, wedging every daemon (vtysh hangs). **A monitor must never harm the monitored.** Fix: the syslog loop only *drains* + non-blocking signal; a separate debounced worker reconciles; `SetReadBuffer(1MB)`. Production: prefer `FRR log file` + inotify tail (the writer never blocks). / shim 绑 `/dev/log`(可靠投递)读得慢会阻塞 FRR 的 `syslog()` 拖垮 daemon。修复=解耦排空+去抖 worker+1MB 缓冲。生产建议改「log file + inotify」。
- **bind-mount inode trap / inode 坑** — `go build -o` makes a new inode; `-v file:/x` binds the old one, so the container runs the stale binary. Use `docker cp`. / 用 `docker cp` 更新容器内二进制。
- **lldpcli watch block-buffering / 块缓冲** — its stdout block-buffers over a pipe; a 15s periodic reconcile is the safety net. / 加 15s 周期兜底。
- **LLDP needs same mount ns as lldpd** (lldpcli uses a Unix socket). / LLDP 需与 lldpd 同 mount ns。
- **⚠️ Unkeyed list elements are matched literally, not as wildcards** — this cache treats `interface` (no key) as an exact path segment, so a precise leaf path like `/interfaces/interface/state/oper-status` returns nothing on Get/Subscribe; only subtree subscriptions match. Collectors must spell out `[key=*]` (see `lab/gnmic-frr.yaml`). / 无键 list 元素按字面匹配、不当通配,采集路径必须显式写 `[key=*]`,否则精确 leaf 返回空。
- **LLDP frames are dropped by Linux/docker bridges** unless `group_fwd_mask=0x4000` is set on each bridge. / 网桥默认丢 LLDP 组播,要设 `group_fwd_mask=0x4000`。
- **FPM/BMP are FRR loadable modules** — zebra needs `-M dplane_fpm_nl`, bgpd needs `-M bmp`, else `fpm address` / `bmp` commands aren't recognized. / FPM/BMP 是模块,要 `-M` 载入。

## Next / 下一步

- **Set** (config push) — turn the read-only monitor into a configurable target; start from a low-risk subset (L2 first, not BGP/OSPF core). / 配置下发,从低风险子集起步。
- Harden OSPF syslog to `log file` + inotify. / OSPF syslog 硬化为 log file + inotify。
- IPv6 / multi-nexthop / AF_MPLS LFIB / more AFT fields. / IPv6、多下一跳、AF_MPLS LFIB、更多 AFT 字段。
