# Metrics reference

Every metric this exporter can emit, with its labels and what it means.

**This file is part of the contract.** Adding, renaming or removing a metric, a
label, or a `--collector.*` flag requires updating this document in the same
change. `scripts/check_config.py` enforces it — a metric declared in the code
but missing here (or listed here but absent from the code) fails the check.

- Namespace: all metrics are prefixed `vmware_`.
- Type: everything is a **gauge** except `vmware_scrape_errors_total`
  (counter) and `vmware_exporter_build_info` (gauge, value always 1).
- `*_info` metrics always have the value `1`. They exist to carry labels, which
  you join onto the numeric series — see [Joining on `_info`
  metrics](#joining-on-_info-metrics).

## Contents

- [Self-monitoring](#self-monitoring)
- [Topology and inventory](#topology-and-inventory) — `datacenter`, `cluster`
- [Hosts](#hosts) — `host`
- [Virtual machines](#virtual-machines) — `vm`
- [Datastores](#datastores) — `datastore`
- [Resource pools](#resource-pools) — `resourcepool`
- [vSAN](#vsan) — `vsan`, `vsan.perf`
- [ESXi CLI](#esxi-cli) — `esxcli.host.nic`, `esxcli.storage`
- [Performance counters](#performance-counters) — dynamic counters from the
  vCenter performance manager, emitted by `host`, `vm` and `datastore`
- [Deprecated metrics](#deprecated-metrics)
- [Common labels](#common-labels)

## Self-monitoring

Emitted on every scrape regardless of which collectors are enabled. These are
the series to alert on: they tell you whether the exporter is working, as
distinct from whether vSphere is healthy.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_up` | — | `1` when the target could be logged into, `0` when it could not. `0` means the scrape produced **no inventory data** — treat every other metric as stale. A wrong password shows up here, not as an HTTP error. |
| `vmware_scrape_duration_seconds` | — | Wall-clock duration of the whole scrape, including login and logout. Compare against your `scrape_timeout`. |
| `vmware_scrape_collector_duration_seconds` | `collector` | Duration of one collector. Use it to find which collector is making a large inventory slow. |
| `vmware_scrape_collector_success` | `collector` | `1` when that collector completed, `0` when it errored. A single failing collector does not fail the scrape. |
| `vmware_scrape_errors_total` | `collector` | **Counter.** Total scrape errors per collector. The label value `login` covers authentication failures. Alert on `rate()`, not on the raw value. |
| `vmware_exporter_build_info` | `version`, `revision`, `branch`, `goversion`, `goos`, `goarch`, `tags` | Always `1`. Answers "which build is this host running?" |
| `vmware_exporter_config_last_reload_successful` | — | `1` when the last `systemctl reload` succeeded, `0` when it failed. **Worth alerting on:** `systemctl reload` exits 0 as long as the signal was delivered, so a rejected configuration is invisible otherwise. A failed reload keeps the previous configuration. |
| `vmware_exporter_config_last_reload_success_timestamp_seconds` | — | Unix time of the last *successful* reload, or of process start if none has happened. |

With `-disable.exporter.metrics=false` the standard Go and process collectors
(`go_*`, `process_*`) are added to `/metrics` as well.

### Suggested alerts

```promql
# the exporter cannot log in -- everything else is stale
vmware_up == 0

# a configuration reload was rejected; the process kept the old config
vmware_exporter_config_last_reload_successful == 0

# one collector is failing while the scrape still "succeeds"
rate(vmware_scrape_errors_total[15m]) > 0
```

## Topology and inventory

Collectors: `datacenter`, `cluster` (both enabled by default).

These are almost all `_info` metrics whose purpose is to give you the parent
references vSphere itself uses — you join them onto the numeric series to answer
"which cluster is this VM in?".

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_target_info` | `target`, `type`, `version`, `build`, `patch` | The scrape target. `type` is `vcenter` or `esxi`, which determines what the other collectors can see: a standalone ESXi host has no clusters, no real datacenter and no vSAN cluster health. Dashboards key their conditional rendering off this metric. |
| `vmware_vcenter_info` | `version`, `build`, `patch`, `vcenter` | Build details of the endpoint. Kept alongside `vmware_target_info` because existing panels already reference it. |
| `vmware_datacenter_info` | `dcmo`, `dc`, `vcenter`, (`synthetic`) | One series per datacenter. On ESXi the only datacenter is the implicit `ha-datacenter` pseudo-object, which is tagged `synthetic="true"` — filter with `{synthetic!="true"}` to exclude it. |
| `vmware_folder_info` | `foldermo`, `dc`, `dcmo`, `vcenter` | One series per `host` or `datastore` folder, for walking the inventory tree. Note `dc` here carries the *folder* name, not the datacenter name. |
| `vmware_cluster_info` | `cmo`, `vmwcluster`, `foldermo`, `vcenter` | One series per cluster. The cluster name label is `vmwcluster`, not `cluster` — `cluster` is a reserved label in many Prometheus setups. |
| `vmware_cluster_datastore` | `cmo`, `vmwcluster`, `dsmo`, `vcenter` | Which datastores a cluster can reach, **one series per datastore**. |
| `vmware_compute_info` | `cmo`, `host`, `foldermo`, `vcenter`, (`synthetic`) | Standalone compute resources — hosts not in any cluster. Only emitted when no cluster exists. On ESXi this is the synthetic `ha-compute-res`; under vCenter a standalone host has a real auto-generated ComputeResource and is *not* tagged synthetic. |
| `vmware_compute_datastore` | `cmo`, `host`, `dsmo`, `vcenter`, (`synthetic`) | Datastores reachable from a standalone compute resource, one series per datastore. |

## Hosts

Collector: `host` (enabled by default).

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_host_info` | `hostmo`, `host`, `cmo`, `vcenter` | One series per ESXi host, with its parent cluster or compute resource. |
| `vmware_host_hardware_info` | `hostmo`, `host`, `vendor`, `model`, `cpu_type`, `vcenter` | Hardware model and CPU type. |
| `vmware_host_software_info` | `hostmo`, `host`, `software`, `version`, `build`, `vcenter` | ESXi version and build — what you group by when planning patching. |
| `vmware_host_cpu_corecount` | `hostmo`, `host`, `vcenter` | Physical CPU cores. |
| `vmware_host_cpu_threadcount` | `hostmo`, `host`, `vcenter` | Physical threads, i.e. cores × SMT width. |
| `vmware_host_cpu_capacity_hertz` | `hostmo`, `host`, `vcenter` | Average core frequency in hertz. **Multiply by `cpu_corecount`** for total host capacity — this is per core, not the host total. |
| `vmware_host_mem_capacity_bytes` | `hostmo`, `host`, `vcenter` | Total physical memory in bytes. |

Host CPU and memory *utilisation* are performance counters, not these metrics —
see [Performance counters](#performance-counters).

## Virtual machines

Collector: `vm` (enabled by default). This is usually the largest collector by
series count: one host has a few series, one VM has several plus its performance
counters.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_vm_info` | `vmmo`, `vm`, `hostmo`, `vcenter` | One series per VM, with the host it runs on. Join on `hostmo` to reach `vmware_host_info`. |
| `vmware_vm_cpu_corecount` | `vmmo`, `vm`, `hostmo`, `vcenter` | vCPUs configured. |
| `vmware_vm_mem_capacity_bytes` | `vmmo`, `vm`, `hostmo`, `vcenter` | Configured RAM in bytes. |
| `vmware_vm_datastore_capacity_used_bytes` | `vmmo`, `vm`, `vcenter`, `dsmo` | Storage this VM commits on a given datastore — disks, logs, snapshots and configuration files. One series per VM/datastore pair, so a VM with disks on three datastores produces three. |
| `vmware_vm_snapshot_info` | `vmmo`, `vm`, `vcenter`, `name` | One series per snapshot; `name` is the snapshot name. **The value is the creation time as a Unix timestamp**, not `1` — so `time() - vmware_vm_snapshot_info` is the snapshot's age, which is the usual thing to alert on. |

```promql
# snapshots older than 7 days
(time() - vmware_vm_snapshot_info) > 7 * 86400
```

## Datastores

Collector: `datastore` (enabled by default).

Static inventory metrics from `Summary`:

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_datastore_accessible` | `dsmo`, `ds`, `vcenter` | `1` when the datastore is reachable, `0` when it is not. |
| `vmware_datastore_capacity_bytes` | `dsmo`, `ds`, `vcenter` | Datastore capacity in bytes. |
| `vmware_datastore_free_bytes` | `dsmo`, `ds`, `vcenter` | Available space in bytes. |
| `vmware_datastore_info` | `dsmo`, `ds`, `type`, `pfinstance`, `foldermo`, `vcenter` | Datastore metadata: `type` (VMFS, NFS, vSAN, etc.), `pfinstance` is the storage pod or protocol endpoint. |

Performance counters (`disk.provisioned.latest`, `disk.used.latest`):

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_datastore_disk_provisioned_bytes` | `vcenter`, `ds`, `dsmo` | Provisioned (allocated) space on the datastore in bytes. Unmapped from `disk.provisioned.latest` — the original `.latest` rollup suffix is stripped. |
| `vmware_datastore_disk_used_bytes` | `vcenter`, `ds`, `dsmo` | Actually used space on the datastore in bytes. |

Both are `kiloBytes`-sourced counters, so the value is `(raw KiB) × 1024`. They are **not** instanced (no `pfinstance` label).

## Resource pools

Collector: `resourcepool` (enabled by default).

One series per resource pool. CPU metrics are in hertz, memory in bytes.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_resourcepool_info` | `rpmo`, `rp`, `parentmo`, `ownermo`, `vcenter`, (`synthetic`) | Resource pool identity, with its parent pool (`parentmo`) and owning cluster or compute resource (`ownermo`). Two descs share this name — the `synthetic="true"` variant is used for the pseudo resource pool that ESXi exposes. They must be separate descs because a label set is part of a desc; one desc cannot sometimes carry `synthetic` and sometimes not without `client_golang` panicking. |
| `vmware_resourcepool_cpu_limit_hertz` | `rpmo`, `rp`, `vcenter` | Configured CPU limit. Not emitted when the pool is unlimited; check `cpu_limited` instead. |
| `vmware_resourcepool_cpu_limited` | `rpmo`, `rp`, `vcenter` | `1` when a CPU limit is configured, `0` when unlimited. |
| `vmware_resourcepool_cpu_max_usage_hertz` | `rpmo`, `rp`, `vcenter` | Maximum CPU usage the pool can reach (its "ceiling"). |
| `vmware_resourcepool_cpu_reservation_hertz` | `rpmo`, `rp`, `vcenter` | Configured CPU reservation. |
| `vmware_resourcepool_cpu_reservation_used_hertz` | `rpmo`, `rp`, `vcenter` | CPU reservation consumed by all descendants. |
| `vmware_resourcepool_cpu_shares` | `rpmo`, `rp`, `level`, `vcenter` | CPU shares. `level` is `low`, `normal`, `high`, or `custom`. |
| `vmware_resourcepool_cpu_unreserved_hertz` | `rpmo`, `rp`, `vcenter` | CPU still available for reservation by VMs. |
| `vmware_resourcepool_cpu_usage_hertz` | `rpmo`, `rp`, `vcenter` | Current CPU usage of the pool and its descendants. |
| `vmware_resourcepool_mem_limit_bytes` | `rpmo`, `rp`, `vcenter` | Configured memory limit. Not emitted when unlimited; check `mem_limited`. |
| `vmware_resourcepool_mem_limited` | `rpmo`, `rp`, `vcenter` | `1` when a memory limit is configured, `0` when unlimited. |
| `vmware_resourcepool_mem_max_usage_bytes` | `rpmo`, `rp`, `vcenter` | Maximum memory usage the pool can reach. |
| `vmware_resourcepool_mem_reservation_bytes` | `rpmo`, `rp`, `vcenter` | Configured memory reservation. |
| `vmware_resourcepool_mem_reservation_used_bytes` | `rpmo`, `rp`, `vcenter` | Memory reservation consumed by all descendants. |
| `vmware_resourcepool_mem_shares` | `rpmo`, `rp`, `level`, `vcenter` | Memory shares. `level` is `low`, `normal`, `high`, or `custom`. |
| `vmware_resourcepool_mem_unreserved_bytes` | `rpmo`, `rp`, `vcenter` | Memory still available for reservation by VMs. |
| `vmware_resourcepool_mem_usage_bytes` | `rpmo`, `rp`, `vcenter` | Current memory usage of the pool and its descendants. |
| `vmware_resourcepool_overall_status` | `rpmo`, `rp`, `status`, `vcenter` | Overall status as reported by vSphere. `status` is `gray`, `green`, `yellow`, or `red`. |
| `vmware_resourcepool_vm` | `rpmo`, `rp`, `vmmo`, `vcenter` | Resource pool to VM mapping, one series per VM. |

## vSAN

Collector: `vsan` (disabled by default). Requires `--collector.vsan`.

Cluster-level capacity, disk, health, and resync metrics from the vSAN API. The
`vmwcluster` label matches `vmware_cluster_info` so you can join on it.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_vsan_enabled` | `cmo`, `vmwcluster`, `vcenter` | `1` when vSAN is enabled on the cluster, `0` when not. Emitted for every cluster, so `0` distinguishes "vSAN is off" from "the vsan collector is not running". |
| `vmware_vsan_capacity_bytes` | `cmo`, `vmwcluster`, `vcenter` | Total vSAN datastore capacity in bytes. |
| `vmware_vsan_capacity_free_bytes` | `cmo`, `vmwcluster`, `vcenter` | Free vSAN capacity. Raw from the API. |
| `vmware_vsan_capacity_used_bytes` | `cmo`, `vmwcluster`, `vcenter` | Used vSAN capacity. Derived as `capacity - free` — the API reports no direct used value. |
| `vmware_vsan_dedup_enabled` | `cmo`, `vmwcluster`, `vcenter` | `1` when deduplication and compression is enabled, `0` when not. |
| `vmware_vsan_disk_capacity_bytes` | `cmo`, `vmwcluster`, `host`, `device`, `vcenter` | Capacity of a vSAN physical disk. |
| `vmware_vsan_disk_capacity_used_bytes` | `cmo`, `vmwcluster`, `host`, `device`, `vcenter` | Used capacity of a vSAN physical disk. |
| `vmware_vsan_disk_health` | `cmo`, `vmwcluster`, `host`, `device`, `uuid`, `state`, `vcenter` | vSAN disk health status as a label. `state` is the summary health. |
| `vmware_vsan_health_status` | `cmo`, `vmwcluster`, `status`, `vcenter` | vSAN cluster health. `status` is `green`, `yellow`, `red`, or `unknown`. |
| `vmware_vsan_resync_bytes` | `cmo`, `vmwcluster`, `vcenter` | Data left to resync, in bytes. Zero means no resync is in progress. Requires vSphere API 6.7+. |
| `vmware_vsan_resync_objects` | `cmo`, `vmwcluster`, `vcenter` | Objects currently syncing. Zero means no resync in progress. |
| `vmware_vsan_resync_recovery_seconds` | `cmo`, `vmwcluster`, `vcenter` | Estimated time to complete the resync. Zero means no resync is in progress. |

### vSAN performance

Collector: `vsan.perf` (disabled by default). Requires `--collector.vsan.perf` and
the vSAN performance service must be enabled on the cluster (it is off by
default in vSphere, and when off vCenter returns empty data rather than an error).

Metrics are named `vmware_vsan_perf_<label>`, where `<label>` comes from a fixed
whitelist — the exporter does not export every counter vSAN offers. The entity
type is a **label value, not part of the metric name**, so
`sum by (entity) (vmware_vsan_perf_iops_read)` works in one line instead of
requiring a join across metric names.

| Metric | Meaning |
|--------|---------|
| `vmware_vsan_perf_iops_read` | Read IOPS. |
| `vmware_vsan_perf_iops_write` | Write IOPS. |
| `vmware_vsan_perf_throughput_read` | Read throughput. |
| `vmware_vsan_perf_throughput_write` | Write throughput. |
| `vmware_vsan_perf_latency_avg_read` | Average read latency, as named on the domclient side. |
| `vmware_vsan_perf_latency_avg_write` | Average write latency, as named on the domclient side. |
| `vmware_vsan_perf_latency_read` | Read latency, as named at disk level. |
| `vmware_vsan_perf_latency_write` | Write latency, as named at disk level. |
| `vmware_vsan_perf_congestion` | vSAN congestion — a bottleneck signal with no vSphere-side equivalent. |
| `vmware_vsan_perf_oio` | Outstanding IO. |
| `vmware_vsan_perf_capacity` | Disk-group level capacity. Distinct from the cluster-level `vmware_vsan_capacity_bytes` above, which is per cluster. |
| `vmware_vsan_perf_capacity_used` | Disk-group level used capacity. |
| `vmware_vsan_perf_capacity_reserved` | Disk-group level reserved capacity. |
| `vmware_vsan_perf_rc_hit_rate` | Read cache hit rate. |
| `vmware_vsan_perf_wb_free_pct` | Write buffer free percentage. |

The same metric name carries slightly different semantics per entity type — for
`cluster-domclient` the value is the cluster total, for `capacity-disk` it is a
single disk. The `entity` label is what distinguishes them.

| Label | Description |
|-------|-------------|
| `cmo` | Cluster managed object ID — join to `vmware_cluster_info`. |
| `vmwcluster` | Cluster display name. |
| `entity` | vSAN performance entity type (e.g. `cluster-domclient`, `host-domclient`, `capacity-disk`). |
| `entityid` | Entity UUID. vSAN's API returns UUIDs only, with no friendly names. |
| `vcenter` | Scrape target. |

Two flags tune this collector:

- `-vmware.vsan.interval` (default `300`) — query window in seconds. vSAN
  statistics land at 5-minute granularity, so values below 300 fetch the same
  single data point repeatedly rather than more detail.
- `-collector.vsan.perf.skip-verify` (default `false`) — query the whitelist
  directly without asking vCenter which entity types it supports. This exists
  because `VsanPerfGetSupportedEntityTypes` does not report every queryable
  entity type, so intersecting with it silently drops entities that actually work.

## ESXi CLI

These collectors issue SOAP calls to each ESXi host individually and are
**disabled by default**. Enable with `--collector.esxcli.host.nic` and
`--collector.esxcli.storage` respectively.

### `esxcli.host.nic`

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_esxcli_host_nic_driver` | `mo`, `host`, `descr`, `driver`, `version`, `firmware` | NIC driver information for the host. One series per NIC. The value is always `1`. |

### `esxcli.storage`

| Metric | Labels | Meaning |
|--------|--------|---------|
| `vmware_esxcli_storage_driver` | `mo`, `host`, `vendor`, `model`, `revision` | Storage device driver info. One series per device. The value is always `1`. |

## Performance counters

The `host`, `vm` and `datastore` collectors emit dynamic performance counters
fetched from the vCenter performance manager (`PerfMgr`). These are **not**
statically registered in `descs.go` — they are generated at scrape time from the
counter metadata returned by vCenter.

### Naming

Counter names are normalised by `translatePerfCounter()` in `perfnames.go`:

| Source name | Normalised name | Notes |
|-------------|-----------------|-------|
| `cpu.usagemhz.average` | `vmware_host_cpu_usage_hertz` | `.average` stripped; override `usagemhz` → `cpu_usage`; unit `megaHertz` → `_hertz` (×1e6). |
| `cpu.ready.summation` | `vmware_host_cpu_ready_seconds_total` | `.summation` stripped; delta → counter with `_total`; unit `millisecond` → `_seconds` (×1e-3). |
| `net.bytesRx.average` | `vmware_host_net_receive_bytes_per_second` | Override `bytesRx` → `receive`; unit `kiloBytesPerSecond` → `_bytes_per_second` (×1024). |
| `datastore.read.average` | `vmware_host_datastore_read_bytes_per_second` | Unit `kiloBytesPerSecond` → `_bytes_per_second` (×1024). |
| `sys.uptime.latest` | `vmware_host_sys_uptime_seconds` | `.latest` stripped; unit `second` → `_seconds` (×1). |
| `cpu.latency.average` | `vmware_host_cpu_latency_ratio` | Unit `percent` → `_ratio` (÷10000). |
| `disk.provisioned.latest` | `vmware_datastore_disk_provisioned_bytes` | Unit `kiloBytes` → `_bytes` (×1024). |

The naming pipeline works as follows:

1. Strip the rollup suffix (`.average`, `.latest`, `.summation`, `.maximum`, `.minimum`).
2. Apply semantic overrides from `perfNameOverrides` (e.g. `bytesRx` → `receive`, `usagemhz` → `usage`).
3. Append the unit suffix from `perfUnitRules` (e.g. `_bytes`, `_seconds`, `_ratio`).
4. For delta counters (StatsType = delta), append `_total` and set type to counter.

### Labels

All performance metric descs carry at least:

| Label | Description |
|-------|-------------|
| `vcenter` | Scrape target. |
| `host` / `hostmo` | Entity name and MOID — for `HostSystem` counters. |
| `vm` / `vmmo` | Entity name and MOID — for `VirtualMachine` counters. |
| `ds` / `dsmo` | Entity name and MOID — for `Datastore` counters. |

When a counter is instanced (e.g. per-NIC `net.bytesRx.average`), an extra label
is added:

| Label | Meaning |
|-------|---------|
| `pfinstance` | The counter instance name from vCenter (e.g. `vmnic0`, `vmhba1`). |

The label order is always `vcenter`, `<entity_name>`, `<entity_moid>`, optionally
`pfinstance`.

### `-metrics.legacy`

With `--metrics.legacy=true` the exporter emits *both* the normalised name and
the original name for every performance counter. The legacy name replaces dots
with underscores directly (e.g. `vmware_host_cpu_usage_average`). The legacy
metric's value is the raw value **before** unit conversion (except for delta
counters, where the bug fix — summing instead of averaging — is applied to both
names).

### Delta counters

Delta (StatsType = delta) counters accumulate over the sampling window and are
exported as Prometheus counters with `_total` suffix. The value is the **sum**
of all samples in the window, not the average. This is a deliberate fix over
the original implementation (which averaged everything).

Non-delta (`absolute`, `rate`) counters are averaged over the window as a
denoising measure.

### Counter lists by collector

**`host`** (common, non-instanced):
`cpu.usagemhz.average`, `cpu.demand.average`, `cpu.latency.average`,
`cpu.entitlement.latest`, `cpu.ready.summation`, `cpu.readiness.average`,
`cpu.costop.summation`, `cpu.maxlimited.summation`,
`mem.entitlement.average`, `mem.active.average`, `mem.shared.average`,
`mem.vmmemctl.average`, `mem.swapped.average`, `mem.consumed.average`,
`sys.uptime.latest`

**`host`** (instanced, per NIC / per datastore):
`net.bytesRx.average`, `net.bytesTx.average`, `net.errorsRx.summation`,
`net.errorsTx.summation`, `net.droppedRx.summation`, `net.droppedTx.summation`,
`datastore.read.average`, `datastore.write.average`,
`datastore.numberReadAveraged.average`, `datastore.numberWriteAveraged.average`,
`datastore.totalReadLatency.average`, `datastore.totalWriteLatency.average`

**`vm`** (common, non-instanced):
Same as `host` common counters.

**`vm`** (instanced):
`net.bytesRx.average`, `net.bytesTx.average`,
`datastore.read.average`, `datastore.write.average`,
`datastore.numberReadAveraged.average`, `datastore.numberWriteAveraged.average`,
`datastore.totalReadLatency.average`, `datastore.totalWriteLatency.average`

**`datastore`** (non-instanced):
`disk.provisioned.latest`, `disk.used.latest`

## Deprecated metrics

These static metrics have renamed counterparts. The old names are still emitted
as soft deprecation — they will be removed in a future release. Only metrics
that were renamed are listed here; performance counter legacy names are gated by
`--metrics.legacy` and are not repeated here.

| Legacy name | Replacement | What changed |
|-------------|-------------|--------------|
| `vmware_host_cpu_capacity` | `vmware_host_cpu_capacity_hertz` | Unit in name (MHz → hertz). |
| `vmware_host_cpu_capacity_mhz` | `vmware_host_cpu_capacity_hertz` | Unit in name (MHz → hertz). |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | Unit in name (MB → bytes). |
| `vmware_vm_mem_capacity` | `vmware_vm_mem_capacity_bytes` | Unit in name (MB → bytes). |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | Unit in name (bytes — the old name was missing the suffix). |
| `vmware_datastore_capacity` | `vmware_datastore_capacity_bytes` | Unit in name (bytes — old name missing suffix). |
| `vmware_datastore_free` | `vmware_datastore_free_bytes` | Unit in name (bytes — old name missing suffix). |

The legacy metrics carry the same labels as their replacements. The value is
identical (the old names already used the correct unit; only the name was
ambiguous).

## Common labels

Labels that appear repeatedly across metrics. MOID stands for "managed object
ID" — the vSphere internal identifier that stays stable across renames.

| Label | Meaning | Appears on |
|-------|---------|------------|
| `vcenter` | Scrape target (hostname or IP). | All metrics. |
| `target` | Same as `vcenter` but only on `vmware_target_info`. | `target_info` |
| `dcmo` | Datacenter MOID. | `datacenter_info`, `folder_info`, `cluster_info`, `compute_info` |
| `dc` | Datacenter display name. | `datacenter_info` |
| `foldermo` | Folder MOID. | `folder_info`, `cluster_info`, `compute_info`, `datastore_info` |
| `cmo` | Cluster or ComputeResource MOID. | `cluster_info`, `cluster_datastore`, `compute_info`, `compute_datastore`, `host_info`, `vsan_*`, `vsan_perf_*` |
| `vmwcluster` | Cluster display name. | `cluster_info`, `cluster_datastore`, `vsan_*`, `vsan_perf_*` |
| `hostmo` | Host MOID. | `host_*`, `vm_*` |
| `host` | Host display name. | `host_*`, `compute_info`, `compute_datastore`, `esxcli_*` |
| `vmmo` | Virtual machine MOID. | `vm_*`, `resourcepool_vm` |
| `vm` | Virtual machine display name. | `vm_*` |
| `dsmo` | Datastore MOID. | `datastore_*`, `cluster_datastore`, `compute_datastore`, `vm_datastore_*` |
| `ds` | Datastore display name. | `datastore_*` |
| `rpmo` | Resource pool MOID. | `resourcepool_*` |
| `rp` | Resource pool display name. | `resourcepool_*` |
| `synthetic` | `"true"` when the object is a vSphere auto-created placeholder (e.g. `ha-datacenter` on ESXi, root resource pool). | `datacenter_info`, `compute_info`, `resourcepool_info` |
| `mo` | Generic MOID — used in esxcli collectors. | `esxcli_*` |
| `pfinstance` | Performance counter instance name (e.g. `vmnic0`). | Performance counters (instanced only). |

You can filter out synthetic objects with `{synthetic!="true"}`. These are
pseudo-objects that vSphere auto-creates and have no real counterpart in the
inventory.
