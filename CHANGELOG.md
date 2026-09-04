# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ✨ Added

- **A landing page that is actually HTML, and an interactive debug console at
  `/debug`.** The page served at `/` used to be a string literal concatenated
  inside `main()` with no `<!DOCTYPE>` and no opening `<html>`/`<body>` tags —
  only the closing ones — so browsers rendered it through error recovery. Both
  pages now come from templates in the new `web` package, embedded with
  `go:embed`; the deployment story is unchanged (still a single static binary,
  still `go build .`).

  `/debug` lets you enter a target and credentials, tick the collectors you
  want, and see the raw exposition output along with the scrape duration and
  per-collector success — useful for confirming credentials and permissions
  before touching `prometheus.yml`. The collector list on both pages is
  generated from the collector registry, so it cannot drift from what the
  binary supports, and it now shows the cost of each collector
  (`per-host serial` for esxcli, `needs vSAN` / `needs perf service` for vSAN)
  rather than only its default state.

  The console is enabled by default and can be removed entirely with
  `-web.debug-console=false`, which makes `/debug` return 404 and also drops
  the link from the landing page. Turning it off is worth considering on any
  listener reachable beyond a workstation: the form lets anyone who can load
  the page make the exporter connect to an arbitrary address with arbitrary
  credentials.

- **A configuration generator at `/config`.** Fill in the vCenters you want to
  scrape and the page emits the three pieces you need: the file service
  discovery target file, the `scrape_configs` job that reads it, and the
  exporter flags to start the process with. It exists because writing this by
  hand is easy to get *plausibly* wrong — the configuration loads, the scrape
  succeeds, and the data is still incorrect.

  Two shapes are offered for the multi-vCenter `/probe` layout:

  - **`relabel` (default)** — the target file carries `__meta_*` labels and the
    job maps them to `__param_*` in `relabel_configs`. Credentials live in one
    file, the mapping in another; adding a vCenter means appending to the
    target file only.
  - **`inline`** — the target file carries `__param_*` labels directly, which
    Prometheus turns into query parameters without any relabel rule. Shorter,
    but every target must repeat every parameter.

  Both emit `instance` explicitly, which is the part worth knowing about:
  `instance` only falls back to `__address__`, and under `/probe` the address
  *is* the exporter, so without an explicit `instance` every vCenter lands on
  the same series and they overwrite each other. The generated `relabel`
  ordering is also load-bearing — the rule that rewrites `__address__` to the
  exporter must come last, or `__param_target` picks up the exporter's own
  address.

  Selecting more than one collector falls back to a job-level `params` list
  instead of an inline label, because `collect[]` has to appear repeatedly to
  carry several values and a YAML mapping cannot repeat a key. Single-vCenter
  setups can instead generate the `/metrics` form, where the credentials are
  flags on the exporter and the scrape config holds no secret at all. That mode
  takes the same vCenter list and turns each entry into its own exporter
  process: the ports count up from the one in the form, the scrape targets are
  those listen addresses rather than the vCenters, and each target carries a
  `vcenter` label — the exporter emits none itself, so otherwise the only thing
  separating two of them is an `instance` like `localhost:9170`.

  Passwords are replaced with `<password>` in the output by default so a
  screenshot or a paste is safe; the redaction can be switched off.

  The page shares `-web.debug-console` with `/debug` — one flag removes both,
  and both then return 404 with no dead links left on the landing page. The
  output is worth running through `promtool check config` before deploying, as
  with anything generated.

- **`/probe` now accepts its parameters in a POST form body**, in addition to
  the query string it has always accepted. The debug console uses this so that
  passwords stay out of the browser address bar, out of browser history, and
  out of the access logs of any reverse proxy that logs query strings. GET with
  a query string keeps working exactly as before — there is a regression test
  guarding it specifically, because the obvious implementation
  (`if r.Method == http.MethodPost { r.ParseForm() }`) silently breaks every
  existing GET request: `r.Form` is empty for GET unless `ParseForm` is called
  unconditionally, so every `/probe?target=...` would have started returning
  `400 target parameter is required`.

  A malformed parameter (a bad percent-escape, say) still does not fail the
  request. `ParseForm` returns an error where `r.URL.Query()` silently dropped
  the offending key; the error is logged and the parameters that did parse are
  used, keeping the previous behaviour for existing callers.

  > Note that the exporter's own `basic_auth_users` (via `-web.config.file`)
  > and passing vCenter credentials through `/probe`'s basic auth are mutually
  > exclusive — both read the same `Authorization` header, and exporter-toolkit
  > does not strip it after validating. Use parameters for the vCenter
  > credentials if you protect the listener this way.

- **`systemctl reload` now applies configuration changes without restarting.**
  The exporter registers a SIGHUP handler that re-reads `-file` and the
  environment variables and writes the values back into the flags the request
  path reads. Because every scrape re-reads the collector switches and every
  login re-reads the `-vmware.*` credentials, the next scrape picks up the new
  configuration — no restart, no gap in the time series. `-log.level` is
  reloadable too, which makes temporarily switching to `debug` non-disruptive.
  `ExecReload=/bin/kill -HUP $MAINPID` is back in the shipped unit.
  (verified end-to-end: `/metrics` stayed HTTP 200 across SIGHUP and the new
  `log.level` took effect)

  Reload keeps the previous configuration when it fails, and never applies a
  partial one — the new values are computed on a shadow flag set first and
  committed only if all of them are valid. Two new metrics make a failed reload
  alertable, which matters because `systemctl reload` still exits 0 (the signal
  *was* delivered): `vmware_exporter_config_last_reload_successful` and
  `vmware_exporter_config_last_reload_success_timestamp_seconds`.

  `-http.address`, `-web.config.file` and `-log.format` cannot be reloaded (the
  listener is bound, TLS is loaded, the log handler type is fixed). Changing
  them is logged as a warning instead of being silently ignored.

### 🔧 Fixed

- **systemd unit no longer kills the service on reload.** The exporter used to
  register no signal handlers, so SIGHUP hit Go's default disposition and
  terminated the process — `ExecReload=/bin/kill -HUP $MAINPID` was a footgun:
  `systemctl reload` reported success while stopping the service. Fixed properly
  by the SIGHUP handler above; the interim fix was to ship the unit without
  `ExecReload`. `scripts/check_config.py` now validates the unit against the
  source in *both* directions — it requires `ExecReload` while the handler
  exists, and rejects it if the handler is ever removed — so the two cannot
  drift apart again. (verified by signalling a running exporter)

- **vmware.conf no longer puts the password into the process cmdline.** The file
  held a single `ARGS="-vmware.password=..."` line that the unit expanded onto
  `ExecStart`, making the password readable by any user on the host via
  `/proc/<pid>/cmdline`. Changed to an `EnvironmentFile` of `VMWARE_<flag>`
  variables, matching the approach `docker-compose.yml` already used. The old
  `ARGS=` form is now rejected by `scripts/check_config.py` as a regression.
  (verified by `ps` inspection + end-to-end run)

- **Deployment files are now included in the release tarball.** The README
  instruction to copy `vmware-exporter.service` to `/etc/systemd/system/` had
  nothing to copy from when installing from a release archive. Both
  `vmware.conf` and `vmware-exporter.service` ship in the archive now.

### 🔐 Security — systemd unit hardened

- `ProtectSystem=full` → `strict`; added `NoNewPrivileges`,
  `SystemCallFilter=@system-service`, empty `CapabilityBoundingSet`,
  `RestrictAddressFamilies` limited to `AF_INET AF_INET6 AF_UNIX`,
  `MemoryDenyWriteExecute`, `LockPersonality`, `ProtectKernel*`,
  `PrivateDevices`, `ProtectClock`, `ProtectHostname`, `RestrictSUIDSGID`,
  `RestrictRealtime`, `RestrictNamespaces`. `LimitNOFILE` drops from 1048576 to
  65536; `LimitCORE=infinity` removed — together they meant a crash could write
  a multi-gigabyte core dump. `StandardOutput=syslog` → `journal` (deprecated
  since systemd 246).

### 🧰 Maintenance

- **The generated configuration is parsed in the test suite, not eyeballed.**
  `scripts/generate_config.js` drives `web/config.js` through a DOM stub so the
  browser code that produces the YAML can be called from Go tests, and the
  output is loaded with `yaml.v3` in strict mode — the same parser Prometheus
  uses, so a duplicate key or an unknown field fails the test rather than the
  deployment. Eight defects were injected and each one turned the suite red
  before being restored: a repeated `__param_collect[]` key, a `relabel` rule
  ordered before the address rewrite, a dropped `instance` label, a redaction
  switch that leaked, a bare `:9169` used as a relabel replacement (which
  yields an empty hostname, so the generator prefixes `localhost`), several
  `/metrics` processes sharing one port, a target file scraping the vCenter
  instead of the exporter, and a missing `vcenter` label. The collector list
  the tests feed in comes from the real registry, so it cannot drift from what
  the binary supports. The tests skip when `node` is absent instead of failing.

- **`scripts/check_config.py` validates the shipped systemd unit and the new
  vmware.conf format.** Two new functions — `check_unit()` and `check_conf()` —
  with 6 reverse-verified defect injections (each injected → specific message →
  restored → clean). `check_conf()` now requires `VMWARE_vmware_vcenter`,
  `VMWARE_vmware_username` and `VMWARE_vmware_password` to be present, so
  emptying the file does not bypass the other checks.

- **`README.md` and `README-zh.md` updated.** Both now document the environment-
  variable-based `vmware.conf`, the `restart`-not-`reload` requirement, and a
  `DynamicUser` fallback for systemd < 232. The old `ARGS=` form is removed from
  both.

- **`vmware.conf` is now an EnvironmentFile of `VMWARE_<flag>` variables.**
  The unit runs with `-envflag.enable -envflag.prefix=VMWARE_`. Place and
  permissions are documented in the file header, including the reason 0600
  root:root is correct despite the unprivileged DynamicUser (systemd reads
  EnvironmentFile as root before dropping privileges).

- **`.goreleaser.yaml` archives now include `vmware.conf` and
  `system/vmware-exporter.service`.** The unit file uses `src: system/...` +
  `dst: .` to flatten the directory structure in the archive.

### 🔐 Security — rotate your vCenter credentials

**This repository shipped working vCenter passwords in plain text. They are in
the git history of a public repository and cannot be removed from it.**

The affected values, and where they were:

| Value | Files | First committed |
| --- | --- | --- |
| `public@123` | `vmware.conf`, `README-zh.md` | 2024-08-26 (`2cc216d`, `06c97fd`, `25b9c4b`) |
| `QOo%zF7AsvJ280.s@sc` | `docker-compose.yml` | 2024-08-26 (`25b9c4b`) |
| `public@12345`, `public@54321` | `README-zh.md` (file_sd example) | 2026-01-16 (`935889c`) |

Internal addresses (`172.16.10.1`, `172.16.10.10`, `172.17.41.101`) were exposed
alongside them.

This release replaces every one of them with a placeholder, but **that only
closes the door going forward.** Anyone who cloned the repository, or who opens
any commit from 2024 onwards on GitHub, can still read the originals. Rewriting
history is not an option here — this repository is a fork, and rewriting would
sever the upstream relationship and break every existing clone.

**If any of these passwords was ever real in your environment, change it in
vCenter now.** Assume it is compromised.

Two things were added to keep this from happening again:

- `-web.config.file` now exists, so the exporter's own listener can require TLS
  and basic auth. `/probe` accepts vCenter credentials as URL parameters and via
  basic auth, and until now there was no way to encrypt that hop. See
  [Securing the exporter](README.md#securing-the-exporter).
- `scripts/check_config.py` fails if a credential reappears in a shipped config
  file, if a password lands on a container command line, or if an environment
  variable name maps to no registered flag. Wire it into CI.

### ⚠️ Breaking changes

These require action before upgrading. Nothing else in this release changes the
wire format of an existing metric.

#### Metrics renamed

| Old | New | Notes |
| --- | --- | --- |
| `vmware_cluster_datastores` | `vmware_cluster_datastore` | Label semantics changed too — see below |
| `vmware_compute_datastores` | `vmware_compute_datastore` | Label semantics changed too — see below |

Both metrics previously joined every datastore moid of the cluster into a single
comma-separated label value:

```
vmware_cluster_datastores{cluster="C0", dsmo="datastore-59,datastore-61,datastore-63", ...} 1
```

They now emit **one series per datastore**:

```
vmware_cluster_datastore{cluster="C0", dsmo="datastore-59", ...} 1
vmware_cluster_datastore{cluster="C0", dsmo="datastore-61", ...} 1
vmware_cluster_datastore{cluster="C0", dsmo="datastore-63", ...} 1
```

The old shape had two problems. You could not join on `dsmo` — only substring
match it — and adding or removing any single datastore changed the label value,
which produced a brand new series and left the previous one as a zombie until it
aged out of the TSDB.

Neither metric is referenced by the dashboards shipped in this repository, so no
dashboard changes are needed. If you have **your own** alerting rules or panels
built on the old names, update them before upgrading — a renamed metric fails
silently, with no error anywhere.

#### Every metric name normalised

All metric names now follow Prometheus conventions. This is the largest breaking
change in this release: **53 business metrics are affected**, and the old names
are off by default.

Run with `-metrics.legacy` to get the old names back alongside the new ones
during migration. See the migration guide in `README.md` for recording rules that
reconstruct the old names, which is a better long-term position than the flag.

> **The dashboards in `dashboards/` were migrated to the new names** — see
> *Bundled dashboards migrated to the new metric names* below. `-metrics.legacy`
> is only needed for dashboards and alerting rules of your own.

**Performance counters.** Names are derived from the counter metadata vCenter
reports, not from a hand-maintained list, so adding a counter cannot silently
keep the old naming style. Three rules:

1. The vSphere rollup suffix (`.average`, `.summation`, `.latest`) is dropped. It
   describes how vCenter aggregates, not what the value is.
2. The unit becomes a name suffix, converted to a Prometheus base unit.
3. Counters vCenter declares as `delta` become real counters with `_total`.

| Old | New | Conversion | Type |
| --- | --- | --- | --- |
| `vmware_{host,vm}_cpu_costop_summation` | `..._cpu_costop_seconds_total` | ×0.001 | counter |
| `vmware_{host,vm}_cpu_demand_average` | `..._cpu_demand_hertz` | ×1e6 | gauge |
| `vmware_{host,vm}_cpu_entitlement_latest` | `..._cpu_entitlement_hertz` | ×1e6 | gauge |
| `vmware_{host,vm}_cpu_latency_average` | `..._cpu_latency_ratio` | ×1e-4 | gauge |
| `vmware_{host,vm}_cpu_maxlimited_summation` | `..._cpu_maxlimited_seconds_total` | ×0.001 | counter |
| `vmware_{host,vm}_cpu_readiness_average` | `..._cpu_readiness_ratio` | ×1e-4 | gauge |
| `vmware_{host,vm}_cpu_ready_summation` | `..._cpu_ready_seconds_total` | ×0.001 | counter |
| `vmware_{host,vm}_cpu_usagemhz_average` | `..._cpu_usage_hertz` | ×1e6 | gauge |
| `vmware_{host,vm}_datastore_numberReadAveraged_average` | `..._datastore_read_operations` | ×1 | gauge |
| `vmware_{host,vm}_datastore_numberWriteAveraged_average` | `..._datastore_write_operations` | ×1 | gauge |
| `vmware_{host,vm}_datastore_read_average` | `..._datastore_read_bytes_per_second` | ×1024 | gauge |
| `vmware_{host,vm}_datastore_write_average` | `..._datastore_write_bytes_per_second` | ×1024 | gauge |
| `vmware_{host,vm}_datastore_totalReadLatency_average` | `..._datastore_read_latency_seconds` | ×0.001 | gauge |
| `vmware_{host,vm}_datastore_totalWriteLatency_average` | `..._datastore_write_latency_seconds` | ×0.001 | gauge |
| `vmware_{host,vm}_mem_active_average` | `..._mem_active_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_mem_consumed_average` | `..._mem_consumed_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_mem_entitlement_average` | `..._mem_entitlement_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_mem_shared_average` | `..._mem_shared_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_mem_swapped_average` | `..._mem_swapped_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_mem_vmmemctl_average` | `..._mem_balloon_bytes` | ×1024 | gauge |
| `vmware_{host,vm}_net_bytesRx_average` | `..._net_receive_bytes_per_second` | ×1024 | gauge |
| `vmware_{host,vm}_net_bytesTx_average` | `..._net_transmit_bytes_per_second` | ×1024 | gauge |
| `vmware_host_net_droppedRx_summation` | `..._net_receive_dropped_total` | ×1 | counter |
| `vmware_host_net_droppedTx_summation` | `..._net_transmit_dropped_total` | ×1 | counter |
| `vmware_host_net_errorsRx_summation` | `..._net_receive_errors_total` | ×1 | counter |
| `vmware_host_net_errorsTx_summation` | `..._net_transmit_errors_total` | ×1 | counter |
| `vmware_{host,vm}_sys_uptime_latest` | `..._sys_uptime_seconds` | ×1 | gauge |
| `vmware_datastore_disk_provisioned_latest` | `..._disk_provisioned_bytes` | ×1024 | gauge |
| `vmware_datastore_disk_used_latest` | `..._disk_used_bytes` | ×1024 | gauge |

Several of the new names are not mechanical translations, because the vSphere
name would have been misleading:

- `net.bytesRx` → `net_receive`, not `net_bytes_rx`. `receive`/`transmit` is the
  established Prometheus vocabulary (`node_network_receive_bytes_total`).
- `datastore.numberReadAveraged` → `datastore_read_operations`. The "Averaged"
  in the vSphere name is not a rollup — the counter is IOPS.
- `datastore.totalReadLatency` → `datastore_read_latency_seconds`. The "total"
  means "kernel plus device", not an accumulated sum; a literal
  `total_read_latency` would read as a counter.
- `mem.vmmemctl` → `mem_balloon`. `vmmemctl` is the internal driver name.
- `cpu.usagemhz` → `cpu_usage_hertz`. The unit was baked into the vSphere name;
  the suffix now comes from the unit table like every other counter.

**Static metrics.**

| Old | New | Conversion |
| --- | --- | --- |
| `vmware_host_cpu_capacity`, `vmware_host_cpu_capacity_mhz` | `vmware_host_cpu_capacity_hertz` | ×1e6 |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | ×1 |
| `vmware_vm_mem_capacity` | `vmware_vm_mem_capacity_bytes` | ×1048576 |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | ×1 |
| `vmware_datastore_capacity` | `vmware_datastore_capacity_bytes` | ×1 |
| `vmware_datastore_free` | `vmware_datastore_free_bytes` | ×1 |

`vmware_host_cpu_capacity_mhz` was itself introduced as a replacement earlier in
this changelog's own Deprecated section. It is deprecated now too: MHz is not a
Prometheus base unit and promlint flags it. Migrating from `_mhz` to `_hertz` is
a multiplication by 1e6.

Two conversions deserve attention because getting them wrong still yields a
plausible number:

- **`percent` is divided by 10000, not 100.** vSphere reports percent in
  hundredths of a percentage point: a raw `100` means 1%. The `*_ratio` metrics
  are in 0..1, so panels need unit `percentunit`, not `percent`.
- **`kiloBytes` is 1024 bytes and `megaBytes` is 1048576.** vSphere documents
  these as binary multiples; using 1000 understates memory by 2.4%.

Counters whose unit is not in the conversion table are emitted under the **old**
name with a warning logged, rather than guessing a suffix. A wrong unit suffix
would put wrong numbers into the TSDB with nothing to signal it.

#### Bundled dashboards migrated to the new metric names

The five dashboards in `dashboards/` now query the new names and work against a
default exporter with no flags. The migration is scripted in
`scripts/migrate_dashboards.py` so it is reproducible and reviewable; the script
takes `--check` (verify only) and `--revert` (restore from its backups).

The names were the easy half. Three other things had to move with them, and every
one of them is invisible in Grafana when it goes wrong:

- **Unit conversions were removed from 126 expressions.** Panels multiplied by
  `1024`, `1000 * 1000`, `1048576` or `8192` to convert the exporter's kiloBytes
  and MHz into bytes and hertz. The exporter now emits base units, so a surviving
  factor would have multiplied the panel by that factor with nothing to indicate
  it. The script therefore refuses to finish if any such factor is left, rather
  than trusting its own rewrite.
- **Panel units were updated to match** — `kbytes`/`mbytes` → `bytes`,
  `KiBs` → `Bps`, `ms` → `s`. Leaving these alone would have relabelled a correct
  number with the wrong suffix: a 32 GiB host reading as "32 EB".
- **The CPU ready and costop panels were rewritten to use `rate()`.** Eight
  expressions divided a summation counter by a hardcoded `20 * 1000` — an assumed
  20-second vSphere granularity — and the input to that division was already
  wrong because of the `*_summation` aggregation bug described below. They now use
  `rate(..._seconds_total[$__rate_interval])`, deriving the window from the query
  step. This is the reason the migration could not be validated by "the numbers
  must match before and after": the old numbers were wrong.

Metric-to-metric mappings are not maintained by hand in the script. They are
generated from the exporter's own `translatePerfCounter` into
`scripts/metric_migration_map.json`, and `TestMigrationMapMatchesImplementation`
fails if the two ever disagree. `scripts/check_config.py` re-runs the dashboard
checks in CI, so a hand-edit that reintroduces an old name or a stale factor is
caught there too.

#### Two pre-existing dashboard bugs fixed

Found while migrating, and unrelated to the rename — both were wrong before this
release too:

- **Network error and drop counts were inflated 8192x.** In `vmware-host-view`'s
  *Network Errors and Drops* panel, all four series (`errorsRx`, `errorsTx`,
  `droppedRx`, `droppedTx`) multiplied by `8192`, which is `1024`
  (kiloBytes → bytes) × `8` (bytes → bits). That is the correct conversion for a
  throughput panel, and this one was evidently copied from the *Network
  Throughput* panel above it — but these series count *packets*, which have no
  such conversion. Now plotted as-is.
- **Panel units did not match the values.** Several panels declared `kbytes`,
  `mbytes`, `KiBs` or `ms` while the queries produced base units, and
  `vmware-vm-view`'s CPU Latency panel kept `max: 100` and thresholds at 60/80
  after moving to `percentunit`, which would have pinned a 0..1 ratio to the
  bottom of a 0..100 axis and left it permanently green. Bounds and thresholds are
  now rescaled with the unit.

#### `*_summation` counters: aggregation was wrong, and they were the wrong type

vCenter declares six counters with `StatsType: delta`, meaning **each sample is
the increment over that sampling interval**:

- `cpu.ready.summation`, `cpu.costop.summation`, `cpu.maxlimited.summation`
- `net.errorsRx/Tx.summation`, `net.droppedRx/Tx.summation`

The exporter averaged the samples in the scrape window, like every other
counter. For delta counters that is wrong: if three consecutive 20-second
intervals each report 100 ms of CPU ready time, the minute contained 300 ms of
ready time, not 100 ms. Averaging discards all but one interval's worth.

Two things made this hard to notice:

1. **The default configuration hides it.** `samples = -vmware.interval /
   -vmware.granularity = 20 / 20 = 1`, and the mean of one sample equals its
   sum. The bug only appears once `-vmware.interval` is raised — which is
   precisely what someone reducing scrape frequency would do.
2. The averaging used `int64` division, so it also truncated: samples of 1, 1
   and 2 averaged to 1 rather than 1.33.

Delta counters are now **summed** and emitted as Prometheus counters with
`_total`. Non-delta counters keep being averaged, but in floating point.

This changes how you query them. The old idiom divided by a hardcoded interval:

```promql
# old — the 20 is -vmware.granularity, hardcoded into the query, and the
# underlying value was already wrong for any window with >1 sample
vmware_host_cpu_ready_summation / (20 * 1000)
```

```promql
# new — rate() derives the interval, correct for any granularity
rate(vmware_host_cpu_ready_seconds_total[$__rate_interval])
```

Recording rules cannot faithfully reconstruct the old `*_summation` gauges,
because their old values were wrong whenever more than one sample fell in the
window. Migrate these to `rate()` rather than aliasing them.

#### Label removed

`vmware_vm_snapshot_info` no longer carries the `created` label.

The label held the snapshot creation time as an RFC3339 string, while the metric
**value** is the same instant as a Unix timestamp — the label was pure
duplication. A timestamp as a label also gives every snapshot its own series,
and those series linger after the snapshot is deleted.

Migration: read the creation time from the metric value instead of the label.
The value is unchanged.

```promql
# before
vmware_vm_snapshot_info{created="2026-08-30T11:04:12Z"}

# after — the value is the Unix timestamp
time() - vmware_vm_snapshot_info > 7 * 86400   # snapshots older than a week
```

This metric is also unreferenced by the bundled dashboards.

#### Flag replaced: `-prom.maxRequests` → `-collector.max-concurrency`

`-prom.maxRequests` is gone. It was a dead parameter: the upstream framework
stored it in `eHandler.maxRequests` and never read the field again, so setting it
had no effect at any value. Passing it now makes the exporter exit with
`flag provided but not defined` — which is the point. A flag that silently does
nothing is worse than one that fails loudly, because the operator believes a
limit is in place.

`-collector.max-concurrency` (default `8`) replaces it and actually bounds two
things:

- how many collectors run in parallel, and
- the fan-out width inside the `esxcli.host.nic` and `esxcli.storage`
  collectors.

The second is the dangerous one. Previously the esxcli collectors started one
goroutine per host with no limit, then another per NIC inside each of those — a
500-host estate with four NICs each produced roughly 2500 concurrent SOAP
requests against a single vCenter. That is a self-inflicted denial of service,
and `-prom.maxRequests` could not stop it no matter what you set.

Set it to `0` to leave the collector layer unbounded; the per-host fan-out keeps
an internal floor regardless, for the reason above.

#### `/probe` no longer returns HTTP 401 when vCenter rejects the credentials

A login failure used to produce `401 Unauthorized` and no metrics at all. That
made two very different situations indistinguishable to Prometheus: *the target
rejected these credentials* and *the exporter itself is broken* both showed up as
a failed scrape with no data.

The request now returns `200` with `vmware_up 0` plus
`vmware_scrape_collector_success{collector="..."} 0` for every enabled
collector. If you alert on the HTTP status of `/probe`, switch to `vmware_up`.

Emitting only `vmware_up 0` would not have been enough: the
`vmware_scrape_collector_success` series would vanish, turning any alert that
reads it from *firing* into *no data*. Those two states behave differently in
Alertmanager.

#### `-disable.default.collectors` was never real

Both READMEs documented this flag. The binary never registered it, so passing it
always failed with `flag provided but not defined`. The tables no longer list it.
To run a subset, disable the defaults individually:
`-collector.datacenter=false -collector.cluster=false ...`.

`scripts/check_config.py` now fails on any flag documented in a reference table
but not registered by the binary, so this cannot recur. That check also caught
its own blind spot: the regex used to extract flag names excluded `-`, which
silently truncated `-collector.max-concurrency` to `-collector.max`.

### Deprecated

Every pre-rename metric name is now behind `-metrics.legacy`, which defaults to
**false**. The legacy names carry a `DEPRECATED:` marker in their help text
pointing at the replacement, and will be removed in a future release.

This replaces the previous release's unconditional dual-write. Emitting both
names by default meant the promise of "a clean, convention-following metric set"
was never actually delivered — every user paid for the migration window whether
they needed it or not. The flag makes it opt-in.

With `-metrics.legacy=true` the legacy names keep **their original values and
their original types**, so a dashboard built on them behaves exactly as before.
The one exception is the delta aggregation fix described above: those values were
wrong, and preserving a wrong number is not backward compatibility. Under the
default configuration (`samples=1`) the old and new aggregations agree anyway.

### Added

- **`-metrics.legacy`** (default `false`) — also emit the pre-rename metric names
  alongside the normalised ones. Needed by the dashboards bundled in this
  repository until they are migrated, and by any of your own panels or rules that
  reference the old names. `README.md` has recording rules that reconstruct the
  old names from the new ones, which is preferable long-term: the aliases live in
  your Prometheus config where you can delete them one at a time.
- **`vmware_up`** — whether the target could be logged into. `0` means the scrape
  produced no inventory data at all. This is not the `up` metric Prometheus
  generates on its own: that one only reports whether the HTTP request succeeded,
  and for a multi-target exporter *HTTP fine, vCenter login rejected* is a
  routine outcome that the built-in `up` reports as success.
- **`vmware_scrape_errors_total{collector}`** — cumulative scrape failure count,
  and the first counter this exporter has ever had. Every one of the 45+ metrics
  before it was a gauge, `vmware_scrape_collector_success` included — and
  `success` can only answer *did the last scrape work*, never *how many times did
  this fail in the last hour*. An intermittently failing vCenter shows up on a
  gauge as flicker between scrapes, and Prometheus samples on `scrape_interval`:
  anything that fails and recovers between two samples is invisible. A counter
  cannot miss it.

  The `collector="login"` series covers authentication failures. Those are
  deliberately **not** attributed to the individual collectors — if they were,
  one expired password would read as N+1 separate failures and drown out the
  signal you actually want, which is *one specific collector is broken*.

  Collectors that have never failed are emitted with the value `0` rather than
  omitted. That matters more than it looks: a series that does not exist while
  everything is healthy makes your alert read *no data* instead of *zero*, and
  when the first failure finally creates the series, `increase()` has nothing to
  compute a delta against — so the very first outage is the one you miss.

  Counts are bucketed by target, so in `/probe` mode a rejected password on one
  vCenter cannot inflate the error count of another.

  ```promql
  # a single collector failing repeatedly
  increase(vmware_scrape_errors_total{collector!="login"}[15m]) > 3

  # credentials rejected
  increase(vmware_scrape_errors_total{collector="login"}[15m]) > 0
  ```
- **`vmware_scrape_duration_seconds`** (no labels) — total scrape duration
  including login and logout. The framework only produced
  `vmware_scrape_collector_duration_seconds{collector="all_collectors"}`, which
  is not what generic exporter alerting rules query. The labelled series is still
  emitted, unchanged, and still excludes login/logout.
- **Standalone ESXi hosts can now be scraped directly**, without a vCenter. The
  target type is detected from `ServiceContent.About.ApiType` and exposed as a
  new `vmware_target_info{target, type}` metric (`type` is `vcenter` or `esxi`).
  Objects that only exist in vCenter (Datacenter, ComputeResource) are emitted
  as synthetic placeholders carrying `synthetic="true"` so parent-reference
  joins keep working. See the *Scrape modes* section of the README for the
  capability limits of ESXi mode.
- A `target_type` template variable in all bundled dashboards, letting you
  filter vCenter targets from ESXi targets. It defaults to `.*`, so existing
  dashboards behave exactly as before.
- `vmware_scrape_collector_success` and a self-monitoring section in the README
  with a ready-made alerting rule.
- `scripts/patch_dashboards.py` for applying the dashboard changes structurally,
  with `--check` (CI-friendly) and `--revert` modes.
- **`-web.config.file`**, wired to
  [exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md),
  enabling TLS and HTTP basic auth on the exporter's listener. The field existed
  in the config struct but was hardcoded to `""`, so there was no way to turn
  either on. Empty by default, so nothing changes unless you point it at a file.
- `scripts/check_config.py`, a CI-runnable guard over the shipped config files.
  Besides scanning for reintroduced credentials, it cross-checks every
  `VMWARE_`-prefixed variable name in `docker-compose.yml` — including the ones
  in the `.env` example in the comments — against the flags the binary actually
  registers. A name that maps to nothing is a genuine bug: envflag ignores it
  without a word. The credential scan walks every text file git tracks rather
  than a curated list, with `CHANGELOG.md` and the script itself excepted by
  name; a whitelist that misses a file fails silently, which is how
  `README-zh.md` kept its passwords through the first pass of this work. The
  flag list comes from building the exporter and reading `--help`, because the
  collector flags are constructed at registration time
  (`fmt.Sprintf("collector.%s", ...)`) and a literal grep of the source reports
  a valid `VMWARE_collector_vm` as unmatched. It also fails when a registered
  flag has no row in either README's reference table, and when an ecosystem
  present in the repository has no `dependabot.yml` entry watching it — that
  last check was added after the GitHub Actions were found a major version
  behind for the second time, both times shortly after a manual review had
  pronounced them current. It found a third undeclared ecosystem immediately
  (`docker`, for the `Dockerfile`).
- Documentation for `-disable.exporter.metrics` and `-disable.exporter.target`
  in README-zh.md, where both were missing entirely. The first **defaults to
  `true`**, so the exporter's own `go_*` and `process_*` metrics are absent
  unless you pass `=false` — measured: 0 series by default, 35 and 5 with the
  flag off. Setting `-disable.exporter.target=true` serves client_golang's
  default registry on `/metrics`, which carries those collectors regardless of
  the other flag, while `vmware_*` disappears entirely (measured: 0 series) and
  must be scraped via `/probe`. Both defaults are now stated in the English
  table too, where they were the only entries without one.
- A *Securing the exporter* section in both READMEs, covering the distinction
  between the vCenter-facing connection and the exporter's own listener, and
  documenting the envflag case rule.
- **`vmware_exporter_build_info` now carries real values.** The metric was always
  exported, but with `version=""` and `branch=""` — because nothing ever passed
  the `-X github.com/prometheus/common/version.*` linker flags. Both
  `.goreleaser.yaml` and the `Dockerfile` now set them.

  The trap here is `revision`: Go stamps it from VCS metadata on its own (1.24
  onwards), so a plain `go build` produces
  `build_info{revision="8b75e41…-modified", version=""}` — populated enough to
  look like it works. It does not. *Which build is this host running* is the most
  common operational question there is, and until now monitoring could not answer
  it.

  The `Dockerfile` takes them as build args, since `.dockerignore` excludes
  `.git` and the build has no access to git metadata:

  ```sh
  docker build --build-arg VERSION=0.2.0 \
               --build-arg REVISION=$(git rev-parse HEAD) \
               --build-arg BRANCH=$(git rev-parse --abbrev-ref HEAD) \
               --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) .
  ```

  Omitting them falls back to empty values — the state before this change, not a
  build failure. A forgotten `--build-arg` should not stop somebody from building
  the image locally.
- **`promlint` gates every metric this exporter emits**, via
  `testutil.CollectAndLint` in two places: `internal/collector` for the
  self-monitoring metrics, and `vmware/collectors` for the 53 business metrics
  (the latter reachable because the test suite already had a govmomi simulator).
  It is Prometheus's own convention checker — missing `_total` on a counter, a
  `_total` on a gauge, non-base units (`milliseconds`, `kilobytes`), camelCase,
  absent help text. None of those break anything at runtime; they break the
  people writing PromQL against them, and by then a rename is a breaking change.

  Four pre-existing violations are exempted with a written reason
  (`vmware_{host,vm}_net_bytes{Rx,Tx}_average` — the camelCase comes straight
  from vCenter's own counter names via `net.bytesRx.average`). The exemption list
  is itself checked: an entry that no longer triggers fails the test, so it
  cannot linger and silently excuse a future regression.
- **`resourcepool` collector** (`-collector.resourcepool`, **default enabled**) —
  18 new metrics covering resource pool limits, reservations, shares and
  instantaneous usage.

  **This adds series to your TSDB without any configuration change on your
  part.** Pass `-collector.resourcepool=false` to opt out. The cardinality is
  dominated by `vmware_resourcepool_vm`, which emits one series per virtual
  machine — the same order of magnitude as `vmware_vm_info`. If you pay per
  series on a hosted Prometheus, budget for that before upgrading.

  It closes the gap between `vmware_vm_info` and the cluster: until now there was
  no way to answer *which resource pool constrains this VM*, because nothing
  exported the intermediate layer. `vmware_resourcepool_vm{rpmo,vmmo}` makes
  `vm → resourcepool → cluster` joinable.

  Unlike telegraf's `inputs.vsphere`, this reads the `ResourcePool` runtime and
  config properties rather than performance counters. One `ContainerView`
  retrieval instead of an extra `QueryPerf` round trip, and — the deciding
  reason — `reservation`, `limit` and `shares` exist **only** in `Config`. Those
  three are what explain *why a VM cannot get CPU*, and no performance counter
  carries them.

  `limit` needs care when reading the metrics: vSphere represents *unlimited* as
  `-1`, and exporting that verbatim would let `limit - usage` return a negative
  number and pollute any `sum()` over it. So an unlimited pool emits **no**
  `cpu_limit_hertz` / `mem_limit_bytes` series at all; use
  `vmware_resourcepool_{cpu,mem}_limited` (`1` = a limit is configured, `0` =
  unlimited) to tell *unlimited* apart from *not collected*. A missing series is
  an empty result in PromQL — safer than a sentinel that arithmetic will happily
  consume as a real value.

  CPU values are converted MHz → hertz (`×1e6`) and configured memory MB → bytes
  (`×1048576`), matching the base-unit convention the rest of the exporter
  follows. Note the asymmetry in vSphere's own API, which the two different
  factors reflect: `Runtime.Memory.*` is already in bytes while
  `Config.MemoryAllocation.*` is in MB.

  On ESXi the implicit `ha-root-pool` is labelled `synthetic="true"`, the same
  treatment `ha-datacenter` and `ha-compute-res` already get.
- **`vsan` collector** (`-collector.vsan`, **default disabled**) — 9 new metrics
  covering vSAN enablement, deduplication, cluster capacity and health, and
  physical disk health.

  **Off by default, unlike every other non-esxcli collector.** Most vSphere
  estates do not run vSAN, and on those the collector would spend two extra SOAP
  round trips per cluster per scrape to learn nothing. Enabling it costs you
  nothing retroactively — no series appear until you pass the flag.

  When vSAN is absent it still emits `vmware_vsan_enabled 0` per cluster and then
  **stops**, issuing no further queries. So `0` means *vSAN is off*, not *the
  collector is not running*. telegraf needs a `vsan_cluster_include` list for
  mixed estates; the early return handles it without a list that goes stale the
  moment someone builds a new vSAN cluster.

  **A read-only account suffices.** This is not incidental — it forced a design
  change. The obvious API for disk health,
  `VsanQueryClusterPhysicalDiskHealthSummary`, requires `EsxRootPassword` in its
  request body: the root password of *every host in the cluster*. No monitoring
  account should hold those, so disk health is read out of the cluster health
  summary response instead (`physicalDisksHealth`), which needs no host
  credentials. Zero extra round trips, and it happens to carry per-disk capacity
  as well. See `docs/DESIGN-resourcepool-vsan.md` §2.2.1.

  `vmware_vsan_health_status` puts the state in a label with a constant value of
  `1`, and **can report `status="unknown"`**. vCenter caches its health summary
  and that cache is empty for a while after a restart or after vSAN is first
  enabled; the collector then re-queries with caching disabled, forcing vCenter
  to actually run the checks. Only if both attempts fail does it emit `unknown`.
  telegraf instead returns silently here, which makes *the health service is
  broken* and *there is no vSAN* indistinguishable — one of those should page
  someone. For the same reason the state is not mapped to `green=0/yellow=1/red=2`
  as telegraf does: that scale has no room for `unknown`.

  `vmware_vsan_capacity_used_bytes` is **derived** as
  `capacity_bytes - capacity_free_bytes`; the API reports no used value. All
  three are exported so the derivation can be checked against the raw numbers.
  (`FreeCapacityB` is `omitempty`, so a missing value yields `used == total` —
  semantically correct: no free space is all space used.)

  Per-disk series carry no `state` or `uuid` on the capacity metrics, only on
  `vmware_vsan_disk_health`. Putting a mutable state into a capacity series' label
  set would mint a new series and orphan the old one every time a disk's health
  flips. Capacity is also omitted entirely when the API reports `0`, rather than
  exporting a zero that reads as *this disk holds nothing*.

  **Resync metrics** — `vmware_vsan_resync_bytes`, `vmware_vsan_resync_objects`
  and `vmware_vsan_resync_recovery_seconds` — ship in the same collector but are
  gated on **vSphere API 6.7 or later**, where `VsanQuerySyncingVsanObjects` was
  introduced. Below that the three series are simply absent, with the reason at
  debug level. The `_seconds` suffix is not a guess: the vSAN Management API
  specifies `totalRecoveryETA` as *"the estimated time in seconds"*. telegraf
  exports it unsuffixed as `total_recovery_eta`, which is fine for InfluxDB but
  not for Prometheus naming.

  All three are emitted **even when zero**, because zero is the healthy state and
  the one operators most want to assert on. Dropping the series while idle would
  leave `absent()` unable to distinguish a quiet cluster from a broken collector.

  Three deliberate departures from telegraf's implementation, all in the same
  function:

  - **The hosts are polled in turn, not just `hosts[0]`.** `VsanSystemEx` has no
    public lookup, so its managed object reference is *derived* from a host's
    (`host-42` → `vsanSystemEx-42`). telegraf takes the cluster's first host and
    stops; if that host happens to be in maintenance mode, resync data vanishes.
    This is the same fallback telegraf itself applies to CMMDS queries — it just
    never applied it here.
  - **The numeric part is validated as numeric.** telegraf only checks that
    splitting on `-` yields two parts, so `host-abc` passes and yields a
    `vsanSystemEx-abc` that cannot exist. Worse, its error path there is
    `return err` where `err` is provably `nil` at that point, so a malformed
    reference is skipped silently and not counted as a failure.
  - **An unparsable API version means no resync query.** telegraf's
    `versionLowerThan` returns `false` ("not lower") when the major version fails
    to parse, so a malformed version string proceeds to call a method that may not
    exist. For an exporter, one missing metric beats a guaranteed-failing round
    trip and an error log on every single scrape.

- **`vsan.perf` collector** (`-collector.vsan.perf`, **default disabled**) — vSAN
  performance statistics: IOPS, throughput, latency, congestion, outstanding IO,
  disk-group capacity and cache-hit rate, per vSAN performance entity.

  **A separate flag from `-collector.vsan`, deliberately.** The health/capacity
  collector makes three light queries per cluster; this one queries CSV
  performance data per entity type and parses it — an order of magnitude more
  work, and far more series. Wanting health and capacity without the performance
  data is a reasonable position, and two flags are what it takes to express it.

  **Requires the vSAN performance service**, which vSphere leaves off by default.
  When it is off vCenter answers with *no data rather than an error*, so the
  symptom is an enabled collector emitting nothing. That case is logged at debug
  level and is explicitly not treated as a collector failure — it must not
  inflate `vmware_scrape_errors_total`.

  **Two hardcoded whitelists, because the API's cardinality is a hazard.** The
  `disk-group` entity type alone exposes 79 metric labels; a ten-host cluster
  with two disk groups per host is 20 x 79 series from that one type. Shipped
  are 5 entity types (`cluster-domclient`, `host-domclient`, `disk-group`,
  `capacity-disk`, `cache-disk`) and 15 metric labels. The label whitelist is
  passed to vCenter as `VsanPerfQuerySpec.Labels`, so excluded metrics are never
  transferred — request-side pruning, not fetch-and-discard. Excluded are the 24
  resync classification counters and the scheduler queue internals.

  Entity types are intersected with `VsanPerfGetSupportedEntityTypes`, so
  unsupported types are never queried and an empty intersection logs a warning
  naming the whitelist rather than returning silently. Because that API does not
  report every queryable entity type (telegraf documents the same gap),
  `-collector.vsan.perf.skip-verify` exists to bypass negotiation.

  **Every metric is averaged over the query window as an instantaneous reading.**
  No delta/rate distinction is attempted. vSAN's `VsanPerfMetricId` does carry
  `statsType` and `rollupType`, but telegraf reads neither — there is no
  field-tested mapping to copy, and inventing one means guessing per label whether
  it is cumulative, with silent numeric errors as the failure mode. The 15
  whitelisted labels are all instantaneous by nature (vSAN's `iops_*` and
  `throughput_*` are already rates, latency is already an average), so the window
  mean is the window's average level. **Do not wrap these in `rate()`.**

  Metric names are `vmware_vsan_perf_<label>` with the entity type in the `entity`
  label rather than the metric name, so `sum by (entity) (...)` works without a
  join across metric names. `entityid` carries the entity UUID; vSAN provides no
  friendly name.

  CSV values are parsed with `ParseFloat(v, 64)`, not telegraf's 32-bit parse —
  32-bit floats carry about 7 significant decimal digits, and throughput in
  bytes/sec passes that on any sizeable cluster. Sample and value counts are
  **checked for equality before iterating**: telegraf indexes `timeStamps[i]` by
  the value index and panics when they disagree, which in an exporter would take
  down the whole scrape. A single unparsable sample is skipped rather than
  discarding the window; a series with no parsable sample is omitted rather than
  exported as `0`.

  New flag `-vmware.vsan.interval` (default 300) sets the query window. It lives
  under `vmware.*` with the other collection parameters rather than opening a new
  top-level namespace for one flag. 300 is not conservatism: vSAN statistics land
  at 5-minute granularity, so a shorter window returns the same single point.

  Two managed object references are **hardcoded string literals** because govmomi
  does not provide them: `vsan-cluster-space-report-system` and
  `vsan-cluster-health-system`. They are transcribed from telegraf's
  `plugins/inputs/vsphere/vsan.go`, cannot be derived from govmomi's type system,
  and cannot be verified against vcsim (its vSAN simulator registers only the
  cluster config and stretched-cluster systems). Changing them requires a real
  vCenter — the source is recorded in the design document as D9.

  Direct ESXi connections skip this collector entirely: the vSAN management
  endpoints live on vCenter, so a standalone host would only ever return 404.

  Tested against a `soap.RoundTripper` stub rather than vcsim, out of necessity:
  vcsim implements 3 vSAN methods and covers only 1 of the 3 this collector uses.
  Every assertion was verified in reverse — the degradation paths, the
  total-minus-free derivation and the cached-then-uncached ordering were each
  confirmed to fail on a deliberately broken build before being trusted.

### Fixed

- **Session leak** on the vCenter API client: failed logins left the session
  open, exhausting the server's session limit over time.
- **Timeout coupling**: a single slow collector could consume the whole scrape
  budget and starve the rest.
- **Divide-by-zero** in the performance-counter averaging path when a counter
  returned no samples.
- **Performance instance label leaking between samples.** The label slice was
  reused across loop iterations, so once `pfinstance` was set it was never
  cleared — a sample without an instance would inherit the previous sample's
  instance and emit a series with a wrong label value, silently.
- **Unknown managed object types were silently collapsed.** The entity-type
  switch had no `default` branch, so an unrecognised type produced metrics
  labelled with `vcenter` only. Every entity of that type overwrote the others,
  leaving one arbitrary series and no indication anything was wrong. Such types
  are now skipped with an error log.
- **Performance interval negotiation was skipped for host and VM collectors.**
  Only the datastore collector used the ESXi-aware fallback, so host and VM
  scrapes against ESXi could request a 300s historical interval that ESXi does
  not serve, and quietly return nothing.
- Three cluster metrics had their help text copy-pasted from
  `vmware_cluster_info` (`"This is basic cluster info to be used for parent
  reference"`) and now describe what they actually measure.
- **`/metrics` ignored every `-collector.<name>` flag.** `metricsHandler` built
  its `collector.Options` without the `Enabled` field, so `NewCollectorSet` fell
  back to each collector's compiled-in default for every one of them. Turning a
  disabled collector on (`-collector.vsan=true`) did nothing — the log said
  `collector disabled` while the command line said otherwise — and turning a
  default-on collector off (`-collector.vm=false`) did nothing either, which is
  the quieter half: the symptom is load on vCenter that cannot be shed, not
  missing data. `/probe` was always correct because it derives `Enabled` from
  the URL parameters, so the two endpoints disagreed about the same flags.
  Found by running the built binary against a simulator rather than by reading
  the code.
- **`.dockerignore` excluded `Changelog.md`, a file that does not exist.** The
  actual file is `CHANGELOG.md`. Docker matches these patterns case-sensitively
  on the daemon, which runs on Linux — so the rule never matched anything, and
  only looked correct when authored on macOS, where APFS is case-insensitive by
  default. The docs, the dashboards and any local `dist/` were all being sent to
  the daemon on every build: 1.1 MB of context for a build that reads none of
  it. Also added `README-zh.md`, which was never listed at all.

### Changed

- **The builder image is pinned to `golang:1.26-alpine3.23`.** It was
  `golang:alpine`, a floating tag that follows the newest Go release. Combined
  with `GOTOOLCHAIN=local` in the official images, that meant the compiler could
  move to an untested minor version without a single line of this repository
  changing — while go.mod declares `go 1.26`. The Go patch level is deliberately
  left floating so security fixes still arrive without a commit.

- **The `prezhdarov/prometheus-exporter` dependency is gone.** Its scheduling and
  configuration layers were replaced by `internal/collector` and
  `internal/config`. This was not a preference for in-house code — four defects
  could not be worked around from the outside:

  1. `Collector.Update()` had no `context.Context` parameter, so no vCenter call
     could ever be cancelled. A client disconnect or a Prometheus scrape timeout
     left the SOAP requests running to completion against vCenter. `-vmware.timeout`
     is now an upper bound on a context derived from the HTTP request, rather than
     the only thing that ever stops a scrape.
  2. A login failure returned immediately, producing zero metrics for the whole
     round — no `up=0`, so the failure could not be alerted on.
  3. `collectorState` was package-private and unreadable from outside, which is
     why `/probe` carried a hand-written duplicate of the entire scheduling loop.
     One implementation now serves both endpoints.
  4. `-prom.maxRequests` was accepted and never read (see *Breaking changes*).

  The three flags the framework owned — `-file`, `-envflag.enable`,
  `-envflag.prefix` — are unchanged and keep working. They are part of the
  documented interface: `docker-compose.yml` relies on `-envflag.enable`
  specifically to keep passwords off the container command line, so dropping them
  would have reopened a leak this release closes.

  `internal/config` also turns three silent failures into errors: an unknown flag
  name in the config file, an invalid `-log.level` or `-log.format` (previously
  downgraded to the default without a word), and a `-file` path that does not
  exist.
- `/metrics` and `/probe` now share one scrape scheduler instead of maintaining
  two divergent code paths.
- The three host-related collectors (`host`, `esxcli.host.nic`,
  `esxcli.storage`) share one `HostSystem` property retrieval per scrape instead
  of issuing three. This is request-scoped sharing, not a cache: the state lives
  on the per-request scrape object, so there is no staleness and no TTL to reason
  about.
- `*prometheus.Desc` objects are built once per namespace instead of once per
  entity per scrape. Entity identifiers moved from `constLabels` to
  `variableLabels`, which is invisible in the exposition format — const and
  variable labels are indistinguishable in Prometheus text output. The hot spot
  was the performance path, where Desc construction scaled with
  *entities × counters* (1000 VMs × 15 counters = 15000 constructions per
  scrape round).
- Dashboard JSON is normalised to 2-space indentation. This was done as a
  separate, semantics-preserving commit so that functional diffs stay reviewable.
- `docker-compose.yml` passes credentials through `environment:` plus
  `-envflag.enable` instead of putting them in `command:`. Anything on a
  container's command line is readable via `docker inspect`, via `ps` inside the
  container, and via `/proc`; environment variables are not.
- `vmware.conf` now documents that it holds a password in plain text, and
  suggests `chmod 600`, a read-only service account, and passing the password via
  an `EnvironmentFile` instead.
- The Docker image builds with `go build -o vmware-exporter .` instead of naming
  `vmware-exporter.go` explicitly. The file-list form compiles only the files
  listed, so adding a second file to package `main` would have dropped it from
  the image without any error — verified with a second file whose `init()` never
  ran in the resulting binary.
- CI now runs `gofmt`, `go vet`, `go test -race`, `golangci-lint` (the
  `.golangci.yml` in the repository had never been executed by any workflow),
  `scripts/check_config.py`, `scripts/patch_dashboards.py --check` and
  `goreleaser check`. The `paths-ignore: '*.md'` filter was dropped: the config
  check scans the READMEs for credentials, so a docs-only change is precisely
  when it needs to run.
- **Release config fixed before it broke a release.** `.goreleaser.yaml` used
  `archives.format` and `archives.format_overrides.format`, both renamed to
  `formats` in goreleaser v2.6 — `goreleaser check` exits non-zero on them, and
  deprecated properties get removed on major versions while
  `release_build.yaml` pins `version: latest`. The release workflow only runs on
  tags, so this would have surfaced during a release; `goreleaser check` now
  runs on every push. Verified with a full `--snapshot` build: four archives,
  `.tar.gz` for Linux and `.zip` for Windows, each containing the binary,
  `LICENSE` and both READMEs.
- Workflow actions brought up to date: `actions/checkout` v3/v4 → v6,
  `actions/setup-go` v3 → v6 (now reading `go-version-file: go.mod` rather than
  a hardcoded `>=1.22.1` that had fallen below go.mod's own `1.26`),
  `goreleaser/goreleaser-action` v4 → v7 — v4 predates the goreleaser v2 that
  `version: latest` installs, against a `version: 2` config file.
  A second pass caught the rest: `golangci/golangci-lint-action` v8 → v9,
  `actions/setup-python` v5 → v7, `docker/build-push-action` v6 → v7,
  `docker/login-action` and `docker/setup-buildx-action` v3 → v4. Every one of
  those majors is the same change — node20 to node24 as the action runtime,
  which GitHub has announced the deprecation of — so they are not optional, and
  none of them touches an input this repository passes.
- `dependabot.yml` watches `github-actions` and `docker` in addition to `gomod`.
  Only Go modules were configured, which is why the action versions above had
  been left behind: nothing was tracking them. Actions are grouped into a single
  PR so the weekly bump stays reviewable. `scripts/check_config.py` now fails if
  an ecosystem present in the tree has no entry here, since finding this by hand
  demonstrably does not work.
- `github.com/vmware/govmomi` v0.55.0 → v0.56.0, with `golang.org/x/sync`
  v0.21.0 → v0.22.0 and `golang.org/x/text` v0.38.0 → v0.41.0 pulled along.
  **This fixes nothing in this exporter** — recording it so the bump is not later
  mistaken for a bugfix. v0.56.0 ships an empty *breaking changes* section, and
  its one correctness fix is a data race in `vcsim` that requires reconfiguring a
  virtual machine's devices to trigger; every test here is read-only collection.
  The bump is dependency hygiene: `x/text` was three minor versions behind, and
  doing it deliberately beats validating the same change again from a dependabot
  PR. Zero source changes — `go.mod` and `go.sum` only.
- Removed dead code: `inSlice`, `moSliceToString`, five entirely
  commented-out files under `vmware/api/` (`clusters.go`, `datastores.go`,
  `host.go`, `vm.go`, `inventory.go` — remnants of a REST `/api/vcenter/...`
  implementation superseded by the SOAP path), and a commented-out manual SOAP
  block in `esxclistoragelist.go` that `esxcli.GetSOAP` replaced.
