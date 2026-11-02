#!/usr/bin/env bash
# Run as root only after node_exporter has its textfile flag and mount.
set -euo pipefail
cd "$(dirname "$0")"
if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root" >&2
  exit 1
fi
if ! id gpu-exporter >/dev/null 2>&1; then
  useradd --system --user-group --no-create-home --shell /usr/sbin/nologin gpu-exporter
fi
# Check device access before installing or enabling the units.
runuser -u gpu-exporter -- /usr/bin/nvidia-smi --query-gpu=uuid,name,utilization.gpu,utilization.encoder,utilization.decoder,memory.used,memory.total --format=csv,noheader,nounits >/dev/null
install -d -o gpu-exporter -g gpu-exporter -m 0755 /var/lib/gpu-exporter
install -d -m 0755 /usr/local/libexec
install -o root -g root -m 0755 gpu-exporter.py /usr/local/libexec/gpu-exporter.py
install -o root -g root -m 0644 gpu-exporter.service gpu-exporter.timer /etc/systemd/system/
systemctl daemon-reload
systemctl start gpu-exporter.service
systemctl enable --now gpu-exporter.timer
