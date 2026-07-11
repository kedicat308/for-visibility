#!/usr/bin/env bash
# setup-telemetry.sh — wire the 5 shims' gnmic-frr collector into the existing
# Prometheus + Grafana stack. Run inside the my-frr VM after deploy-shim.sh.
set -euo pipefail

TELEM=/home/fanwei.guest/arista/telemetry
LAB=/Users/fanwei/arista/frr-visible/lab

# 1. gnmic-frr collector — dual-homed: frr-mgmt (reach the 8 shim :9339) +
#    campus-mgmt (so Prometheus can scrape :9806). Recreate to pick up new targets.
docker rm -f gnmic-frr >/dev/null 2>&1 || true
docker run -d --name gnmic-frr --network frr-mgmt --ip 172.31.0.30 \
  -v "$LAB/gnmic-frr.yaml:/app/gnmic.yaml:ro" --restart unless-stopped \
  ghcr.io/openconfig/gnmic:latest --config /app/gnmic.yaml subscribe >/dev/null
docker network connect campus-mgmt gnmic-frr --ip 172.30.30.20
echo "gnmic-frr started (frr-mgmt 172.31.0.30 + campus-mgmt 172.30.30.20)"

# 1b. pathtrace-exporter — gNMI-native path trace as Prometheus metrics. Runs the
#     same walk as pathtrace-gnmi.sh for configured flows; dual-homed like gnmic-frr
#     (frr-mgmt to reach the shims, campus-mgmt so Prometheus can scrape :9808).
SRC=/Users/fanwei/arista/frr-visible
INV="pe1=172.31.0.11,pe2=172.31.0.12,p1=172.31.0.21,p2=172.31.0.22,ce1=172.31.0.101,ce2=172.31.0.102,ce3=172.31.0.103,ce4=172.31.0.104"
FLOWS="${PATHTRACE_FLOWS:-ce1-ce4:ce1>10.255.1.4,ce4-ce1:ce4>10.255.1.1,ce2-ce3:ce2>10.255.1.3}"
( cd "$SRC" && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -o /tmp/pathtrace-exporter ./cmd/pathtrace-exporter )
docker rm -f pathtrace-exporter >/dev/null 2>&1 || true
docker run -d --name pathtrace-exporter --network frr-mgmt --ip 172.31.0.31 \
  -v /tmp/pathtrace-exporter:/pathtrace-exporter:ro \
  -e INVENTORY="$INV" -e FLOWS="$FLOWS" -e INTERVAL=15s -e LISTEN=":9808" \
  --entrypoint /pathtrace-exporter --restart unless-stopped \
  ghcr.io/openconfig/gnmic:latest >/dev/null
docker network connect campus-mgmt pathtrace-exporter --ip 172.30.30.21
echo "pathtrace-exporter started (frr-mgmt 172.31.0.31 + campus-mgmt 172.30.30.21); flows: $FLOWS"

# 1c. Tempo — trace backend for the cross-device convergence traces (Zipkin +
#     OTLP receivers, local storage). Grafana queries it via the Tempo datasource.
docker rm -f tempo >/dev/null 2>&1 || true
docker run -d --name tempo --network campus-mgmt --user 0 --restart unless-stopped \
  -v "$LAB/tempo.yaml:/etc/tempo.yaml:ro" grafana/tempo:latest -config.file=/etc/tempo.yaml >/dev/null
cat > "$TELEM/grafana/provisioning/datasources/tempo.yml" <<'EOF'
apiVersion: 1
datasources:
  - name: Tempo
    type: tempo
    uid: tempo
    access: proxy
    url: http://tempo:3200
    editable: true
EOF
echo "tempo started (campus-mgmt) + Grafana Tempo datasource provisioned"

# 1d. trace-aggregator — stitch each shim's :9340/traces into cross-device
#     distributed traces and export them to Tempo (Zipkin). Dual-homed: frr-mgmt
#     to pull the shims, campus-mgmt to reach Tempo. See design.md §15.7/§15.8.
( cd "$SRC" && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -o /tmp/trace-aggregator ./cmd/trace-aggregator )
docker rm -f trace-aggregator >/dev/null 2>&1 || true
docker run -d --name trace-aggregator --network frr-mgmt --ip 172.31.0.32 \
  -v /tmp/trace-aggregator:/trace-aggregator:ro \
  -e INVENTORY="$INV" -e WINDOW=1.5s -e INTERVAL=3s -e LISTEN=":9341" \
  -e TEMPO_ZIPKIN="http://tempo:9411/api/v2/spans" \
  --entrypoint /trace-aggregator --restart unless-stopped \
  ghcr.io/openconfig/gnmic:latest >/dev/null
docker network connect campus-mgmt trace-aggregator --ip 172.30.30.22
echo "trace-aggregator started (frr-mgmt 172.31.0.32 + campus-mgmt 172.30.30.22 -> Tempo)"

# 2. Prometheus scrape job for gnmic-frr (idempotent)
if ! grep -q "gnmic-frr" "$TELEM/prometheus.yml"; then
  cat >> "$TELEM/prometheus.yml" <<'EOF'

  # 5-node FRR+shim fleet via gnmic-frr (frr-visible)
  - job_name: gnmic-frr
    static_configs:
      - targets: ['gnmic-frr:9806']
EOF
  echo "added gnmic-frr scrape job"
else
  echo "prometheus scrape job already present"
fi
if ! grep -q "pathtrace-exporter" "$TELEM/prometheus.yml"; then
  cat >> "$TELEM/prometheus.yml" <<'EOF'

  # gNMI-native path trace (frr-visible)
  - job_name: pathtrace-exporter
    static_configs:
      - targets: ['pathtrace-exporter:9808']
EOF
  echo "added pathtrace-exporter scrape job"
fi

# 3. Provision the dashboard into Grafana
cp "$LAB/frr-visible-dashboard.json" "$TELEM/grafana/provisioning/dashboards/frr-visible.json"
echo "dashboard provisioned"

# 4. (Re)start Prometheus so it reloads the config; nudge Grafana too
docker start prometheus >/dev/null 2>&1 || true
docker restart prometheus >/dev/null 2>&1 || true
docker restart grafana   >/dev/null 2>&1 || true

echo "== done =="
echo "   Grafana:    http://localhost:3000  (dashboard: FRR-visible; Explore -> Tempo for traces)"
echo "   Prometheus: http://localhost:9090"
echo "   Traces:     path metrics on the dashboard's Trace row; convergence traces in Tempo"
