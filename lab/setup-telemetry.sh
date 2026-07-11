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

# 3. Provision the dashboard into Grafana
cp "$LAB/frr-visible-dashboard.json" "$TELEM/grafana/provisioning/dashboards/frr-visible.json"
echo "dashboard provisioned"

# 4. (Re)start Prometheus so it reloads the config; nudge Grafana too
docker start prometheus >/dev/null 2>&1 || true
docker restart prometheus >/dev/null 2>&1 || true
docker restart grafana   >/dev/null 2>&1 || true

echo "== done =="
echo "   Grafana:    http://localhost:3000  (dashboard: FRR-visible)"
echo "   Prometheus: http://localhost:9090"
