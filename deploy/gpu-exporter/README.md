# NVIDIA GPU textfile exporter

Optional companion for the Network plugin's host-load tile. It samples
host-wide NVIDIA GPU use (every process: media servers, transcoders, photo
apps) every 15 seconds and writes it where node_exporter's textfile collector
reads it. Python standard library only, one fixed `nvidia-smi` command with a
three-second timeout, no arguments taken from anywhere.

Unsupported fields are left out rather than reported as idle. A failed query
replaces the file with a comment, so the dashboard shows the GPU as unavailable
instead of a stale reading. Each sample carries its own timestamp, and the
plugin ignores files older than 90 seconds.

## Install

On the GPU host, as root:

1. Give node_exporter a textfile directory. In its compose service:

   ```yaml
   command:
     - --path.rootfs=/host
     - --collector.textfile.directory=/textfile
   volumes:
     - /:/host:ro,rslave
     - /var/lib/gpu-exporter:/textfile:ro
   ```

   Recreate node_exporter. Prometheus needs no change: it already scrapes
   node_exporter.

2. Copy this folder to the host and run the installer (safe to re-run):

   ```sh
   bash install.sh
   ```

   It creates a `gpu-exporter` system user, checks that this user can query the
   GPU, installs the script and both units, takes a first sample and enables
   `gpu-exporter.timer`.

3. In the Network plugin's settings, set `load_instance` to node_exporter's
   instance label on this host. The tile shows CPU, load and memory, plus the
   GPU once samples arrive.

The query must work without privileges. If device permissions refuse it, look
at the groups on `/dev/nvidia*` and add `gpu-exporter` to that existing group
only; it never needs sudo or Docker access.

## What it writes

`/var/lib/gpu-exporter/gpu.prom`, replaced atomically on each run. Series are
labelled `uuid` and `name`:

| Metric | Meaning |
|---|---|
| `plex_gpu_util_pct` | GPU utilisation, 0–100 |
| `plex_gpu_encoder_pct` | Video encoder utilisation, 0–100 |
| `plex_gpu_decoder_pct` | Video decoder utilisation, 0–100 |
| `plex_gpu_mem_used_bytes` | Memory in use |
| `plex_gpu_mem_total_bytes` | Total memory |
| `plex_gpu_sample_timestamp_seconds` | When the sample was taken |

The `plex_gpu_` prefix is the name the plugin queries; it covers the whole GPU,
not one application.

## Remove

```sh
systemctl disable --now gpu-exporter.timer
rm /etc/systemd/system/gpu-exporter.{service,timer} /usr/local/libexec/gpu-exporter.py
systemctl daemon-reload
rm -r /var/lib/gpu-exporter
```

Then drop the textfile flag and mount from node_exporter if nothing else uses
them.
