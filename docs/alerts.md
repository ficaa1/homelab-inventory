# Alerts

The alerts below are built in. To hide one you already know about, **silence** it
from the alert (optionally until a date, with a reason). A silenced alert stays
visible but stops counting and notifying; it does not fix anything.

| Rule | Severity | Trigger |
| --- | --- | --- |
| host.stale | warning / critical | Last inventory older than 45 minutes / 2 hours. |
| host.schema | info | Inventory schema differs from the API schema. |
| agent.outdated | info | Agent revision is older than the current revision. |
| zfs.pool.health | critical | Pool is not healthy. |
| zfs.pool.capacity | warning / critical | Pool capacity reaches 80% / 90%. |
| zfs.pool.errors | critical | Pool reports data errors. |
| zfs.vdev.errors | critical | A vdev reports errors. |
| zfs.pool.scrub | warning | Scrub missing, cancelled, or older than 35 days. |
| zfs.snapshot.none | warning | Non-boot pool has no snapshots. |
| zfs.snapshot.stale | warning | Newest pool snapshot is at least 14 days old. |
| proxmox.guest.autostart | warning | Running guest will not start on boot. |
| proxmox.storage.thinpool | warning / critical | Thin pool reaches 75% / 85%. |
| proxmox.storage.capacity | warning / critical | Storage reaches 85% / 95%. |
| proxmox.storage.allocation | info | LVM allocation reaches 95%. |
| proxmox.backup.no_storage | warning | Guests exist but no storage holds backups. |
| proxmox.backup.no_job | warning | Guests exist but no enabled backup job exists. |
| proxmox.backup.unscheduled | warning / info | Guest is not covered by a job; info when not set to start on boot. |
| proxmox.backup.missing | warning / info | Guest has no backup; info when not set to start on boot. |
| proxmox.backup.stale | warning / info | Newest guest backup is at least 8 days old; info when not set to start on boot. |
| systemd.unit.failed | warning | Unit is failed. |
| docker.container.unhealthy | warning | Container is unhealthy or unexpectedly stopped. |
| docker.container.restarting | warning | Container is restarting, or an established container has very short uptime. |
| docker.image.floating | info | Containers use untagged or latest images without a digest. |
| firewall.unknown | info | Packet filter could not be read. |
| firewall.no_inbound_filtering | info | Exposed TCP ports exist without inbound filtering. |
| host.filesystem.capacity | warning / critical | Non-ZFS filesystem reaches 85% / 95%. |
| host.filesystem.readonly | critical | A normally writable local filesystem is unexpectedly read-only. |
| host.kernel.io_errors | critical / warning | Recent kernel storage errors; unknown device is warning. |
| host.disk.smart.failed | critical | SMART health check failed. |
| host.disk.smart.errors | warning | Pending, uncorrectable or media error counters are nonzero. |
| host.disk.nvme.wear | warning | NVMe endurance used reaches 90%. |
