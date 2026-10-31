# Overview load contract

The load source accepts only the configured Prometheus base URL and exact
`job`/`instance` label values. It sends fixed node exporter expressions for
CPU, load, memory and the generic `plex_gpu_*` textfile metrics; callers cannot
provide PromQL. Instant readings older than 90 seconds or more than five seconds
in the future are discarded. Optional one-hour sparklines use the fixed range
queries and never replace valid instant readings when unavailable.

Requests run concurrently under a three-second deadline, disable redirects and
limit response bodies to 1 MiB. Successful and unavailable reports are cached
for 15 seconds, and cancellation is never cached as an outage.
