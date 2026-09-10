
# vmware-exporter

[![Go Report Card](https://goreportcard.com/badge/github.com/prezhdarov/vmware-exporter)](https://goreportcard.com/report/github.com/prezhdarov/vmware-exporter)

This is a simple prometheus exporter that collects various metrics from a vCenter or from a standalone ESXi host.

[中文文档](./README-zh.md)

## How to use

Run the exporter in a docker container (or start as a process) with all the settings necessary. Scrape it..

Exporter scrapes the target configured at startup when the `/metrics` path is used. Multiple targets can be scraped through `/probe?target=host:port`, each with its own credentials supplied either as request parameters (`username` / `password`) or via HTTP Basic Auth.

## Scrape modes

| Mode | Endpoint | Credentials | When to use |
| :--- | :--- | :--- | :--- |
| **Single vCenter** | `/metrics` | Startup flags (global) | One vCenter, simplest deployment |
| **Multiple vCenters** | `/probe?target=...` | Per-request params or Basic Auth | Several vCenters with different credentials |
| **Standalone ESXi** | either | Same as above | Hosts without a vCenter, or direct-to-host collection |

`/debug` is not a scrape mode — it is an interactive page for trying a target out
before adding it to `prometheus.yml`. See [The debug console](#the-debug-console).
`/config` generates the `prometheus.yml` job and the file service discovery
targets for either mode. See [The configuration generator](#the-configuration-generator).

**Target type is detected automatically** by reading `ServiceContent.About.ApiType`
(`VirtualCenter` / `HostAgent`) right after login. No extra flag, no separate
endpoint, no dedicated `scrape_config` — point the exporter at an ESXi host and
it just works.

The detected type is exposed as a metric so dashboards and alerts can branch on it:

```
vmware_target_info{target="10.0.0.5:443", type="esxi", version="7.0.3", build="21930508"} 1
```

> `vmware_vcenter_info` is still emitted unchanged for backwards compatibility
> with existing dashboards.

### Direct ESXi example

```bash
./vmware-exporter \
  -vmware.vcenter="10.0.0.5:443" \
  -vmware.username="root" \
  -vmware.password="your_password" \
  -vmware.insecureTLS \
  -http.address=":9169"
```

Scraping a mix of vCenters and standalone hosts from a single job:

```yaml
scrape_configs:
  - job_name: vmware
    metrics_path: /probe
    static_configs:
      - targets:
          - 10.0.0.1:443   # vCenter
          - 10.0.0.5:443   # standalone ESXi, nothing special needed
    params:
      insecure: ["true"]
    basic_auth:
      username: readonly@vsphere.local
      password: your_password
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter-host:9169
```

If vCenter and ESXi credentials differ, split them into two jobs with their own `basic_auth`.

### What ESXi mode cannot give you

These gaps come from the vSphere object model, not from the exporter:

| Capability | vCenter | Direct ESXi | Notes |
| :--- | :--- | :--- | :--- |
| Datacenter / Cluster | Real objects | **Synthetic** | ESXi only has the implicit `ha-datacenter` / `ha-compute-res`; those metrics carry `synthetic="true"` |
| Host / VM metrics | Full | Full | No difference |
| Datastore capacity | Full | Full | No difference |
| Datastore performance counters | Full | Limited | Counters such as `disk.provisioned.latest` rely on vCenter's historical rollup, which ESXi does not run |
| Sampling interval | Real-time or 5-minute rollup | **Real-time only** | ESXi keeps no historical statistics, so the requested `-vmware.interval` is overridden by the server's `RefreshRate` |
| esxcli collection | Proxied through vCenter | Direct | Uses the SOAP `vim.EsxCLI.*` interface — **not SSH** |
| vSAN metrics | Full | **None** | The vSAN management endpoints live on vCenter (`/vsanHealth`); a standalone ESXi host does not serve them. The `vsan` collector detects this and skips itself, logging at debug level rather than producing errors |

**About the `synthetic` label**: `vmware_datacenter_info` and `vmware_compute_info`
are still emitted on ESXi so that dashboard queries joining on `dcmo` / `cmo` keep
working, while `synthetic="true"` makes it visible at the metric level that these
are not real objects. Filter with `synthetic!="true"` to count real datacenters only.

## Settings 

The exporter can be configured via command line options, environment variables, a yaml config file or a combination of all three. The environment variables set will be overwritten by the contents of the config file, which then will be overwritten by any command line option set at startup. 
The options available are:

| key | description |
| --- | ----------- |
| -envflag.enable | Tells the exporter to use enviromnent flags in its configuration |
| -envflag.prefix | This allows to prefix the environment variables that will be used for configuration | 
| -file | Path to a yaml configuration file that follows the structure of command line options |
| -http.address | The address and port the exporter will bind to in host:port format (default: ":9169") |
| -log.format | Can be either json or logfmt (default: logfmt) |
| -log.level | One of debug,info,warn or error (default: debug) - Don't expect much..|
| -web.config.file | Path to a web configuration file enabling TLS and/or HTTP basic auth on the exporter's own listener - see [Securing the exporter](#securing-the-exporter) |
| -web.debug-console | Serves the interactive pages on `/debug` and `/config` (default: **true**). Pass `=false` to remove both routes entirely - see [The debug console](#the-debug-console) |
| -collector.max-concurrency | Maximum number of collectors running in parallel, and the fan-out width used inside the esxcli collectors (default: 8). Use 0 to leave the collector layer unlimited; the per-host fan-out keeps a built-in floor. Replaces `-prom.maxRequests`, which was accepted but never had any effect |
| -disable.exporter.metrics | Disables the exporter's own `go_*` and `process_*` metrics (default: **true**, so they are absent unless you pass `=false`) |
| -disable.exporter.target | Disables exporter default target - /metrics will only return exporter data - use /probe. `/metrics` then serves client_golang's default registry, which carries the Go and process collectors regardless of the flag above |
| -metrics.legacy | Also emit the pre-rename metric names alongside the normalised ones (default: false). Enable this if you have your own dashboards or alerting rules referencing the old names, see [Metric naming](#metric-naming). The dashboards bundled in this repository use the new names and do not need it |
| -collector.datacenter | Enables or disables DataCenter metrics collection (default: enabled) |
| -collector.cluster | Enables or disables Cluster metrics collection (default: enabled) |
| -collector.datastore | Enables or disables Datastore metrics collection (default: enabled) |
| -collector.host | Enables or disables Host metrics collection (default: enabled) |
| -collector.vm | Enables or disables Virtual Machine metrics collection (default: enabled) |
| -collector.resourcepool | Enables or disables Resource Pool metrics collection: limits, reservations, shares and instantaneous usage (default: enabled) |
| -collector.vsan | Enables or disables vSAN metrics collection: enablement, deduplication, cluster capacity and health, physical disk health, and resync progress (default: **disabled**). Requires a vCenter connection; skipped entirely when connected directly to an ESXi host. Resync metrics additionally require vSphere API 6.7 or later |
| -collector.vsan.perf | Enables or disables vSAN performance metrics collection: IOPS, throughput, latency, congestion and disk-group capacity per vSAN entity (default: **disabled**). Requires a vCenter connection **and** the vSAN performance service enabled on the cluster; independent of `-collector.vsan` |
| -collector.vsan.perf.skip-verify | Skips vSAN performance entity type negotiation and queries the built-in whitelist directly (default: false). Needed because `VsanPerfGetSupportedEntityTypes` does not report every queryable entity type |
| -collector.esxcli.host.nic | Collects ESXi NIC firmware information using esxcli over the SOAP API (proxied by vCenter, or direct when connected to an ESXi host) (default: disabled) |
| -collector.esxcli.storage | Collects ESXi storage firmware information using esxcli over the SOAP API (proxied by vCenter, or direct when connected to an ESXi host) (default: disabled) |

> **`-disable.default.collectors` never existed.** Earlier revisions of this
> table listed it, but the binary has never registered such a flag — passing it
> makes the exporter exit with `flag provided but not defined`. To run only a
> chosen subset, disable the defaults explicitly:
> `-collector.datacenter=false -collector.cluster=false -collector.datastore=false -collector.host=false -collector.vm=false -collector.resourcepool=false`.
>
> `scripts/check_config.py` now fails on any flag that appears in these tables
> without being registered, so this class of drift cannot come back.
| -vmware.granularity | Time granularity of the sampled data in seconds. Must be > 0 and no greater than -vmware.interval (default 20) |
| -vmware.insecureTLS | Trust insecure TLS certificates (true) or verify them (default). ESXi hosts ship self-signed certificates, so this is usually needed for direct collection |
| -vmware.interval | PerfManager sampling window in seconds. This is a *request* - the effective interval is decided by the server's PerfProviderSummary.RefreshRate. No longer used for timeout calculation (default 20) |
| -vmware.timeout | Overall timeout in seconds for a single scrape, covering login, property retrieval and performance sampling (default 60) |
| -vmware.password | Password for the user above |
| -vmware.schema | Use HTTP or HTTPS (default "https") |
| -vmware.username | Username to login with |
| -vmware.vcenter | Target address in host:port format. Accepts a vCenter **or** a standalone ESXi host. This is not the vCenter Management Console. The flag name is kept for backwards compatibility |
| -vmware.vsan.interval | Time window in seconds for vSAN performance queries (default 300). vSAN statistics are collected at a 5-minute granularity, so values below 300 do not yield more data points. Only used by `-collector.vsan.perf` |

Invalid values (for example `-vmware.granularity=0`, or a granularity larger than
the interval) make the process exit at startup with an explicit reason instead of
running with a broken configuration.

### The vSAN collector

`-collector.vsan` is **off by default**, unlike every other non-esxcli collector.
That is not caution for its own sake: most vSphere estates do not run vSAN, and on
those the collector would spend two extra SOAP round trips per cluster per scrape
(capacity and health) to learn nothing. When vSAN *is* absent it still emits
`vmware_vsan_enabled 0` for each cluster and stops there, so `0` means "vSAN is
off" rather than "the collector is not running".

Permissions: a **read-only** account is enough. The collector deliberately avoids
`VsanQueryClusterPhysicalDiskHealthSummary`, whose request body requires
`EsxRootPassword` — the root password of every host in the cluster. Physical disk
health is read out of the cluster health summary instead, which needs no host
credentials. See `docs/DESIGN-resourcepool-vsan.md` §2.2.1.

Two behaviours worth knowing before you write alerts on this:

- **`vmware_vsan_health_status` can report `status="unknown"`.** vCenter caches its
  health summary, and the cache is empty for a while after a restart or after vSAN
  is first enabled. The collector then re-queries with the cache disabled, which
  forces vCenter to actually run the health checks. If both attempts fail it emits
  `unknown` rather than dropping the series — a broken health service and an absent
  cluster should not look identical on a dashboard.
- **Per-disk metrics may be missing while cluster health is fine.** The disk data is
  an optional part of the health summary response. If your vCenter returns it empty,
  `vmware_vsan_disk_health` and the two `vmware_vsan_disk_capacity_*` series will be
  absent while `vmware_vsan_health_status` keeps working normally.

`vmware_vsan_capacity_used_bytes` is derived as `capacity_bytes - capacity_free_bytes`;
the API reports no used value directly. All three are exported so you can check the
derivation against the raw numbers.

**Resync metrics need vSphere API 6.7 or later.** `vmware_vsan_resync_bytes`,
`vmware_vsan_resync_objects` and `vmware_vsan_resync_recovery_seconds` report how much
data the cluster is still rebuilding. On older vCenters the underlying API does not
exist, so the three series are simply absent and the reason is logged at debug level.

All three are emitted **even when they are zero** — zero is the normal, healthy state
("nothing is resyncing"), and that is exactly what you want to be able to assert on.
Dropping the series when idle would make `absent()` unable to tell a healthy cluster
from a broken collector. `..._recovery_seconds` is in seconds, as specified by the
vSAN Management API.

One implementation detail that leaks into behaviour: the managed object these metrics
come from has no public lookup, so its reference is **derived from a host's** managed
object id. The collector therefore tries the cluster's powered-on hosts in turn until
one answers, which keeps resync data available while individual hosts are down or in
maintenance mode.

### The vSAN performance collector

`-collector.vsan.perf` is separate from `-collector.vsan` on purpose. The health and
capacity collector makes three light queries per cluster; this one queries CSV
performance data per entity type and parses it, which costs an order of magnitude
more. You may well want health and capacity without the performance series.

**Prerequisite the flag cannot check for you:** the cluster must have the **vSAN
performance service** enabled (it is off by default in vSphere). When it is off,
vCenter answers the query with *no data rather than an error*, so the symptom is an
enabled collector that emits nothing. The collector logs this at debug level; if you
see no `vmware_vsan_perf_*` series, check the performance service first.

**Cardinality.** Metric names come from vSAN metric labels, and the API is generous:
the `disk-group` entity type alone exposes 79 labels. A ten-host cluster with two
disk groups per host would be 20 entities x 79 series from that one entity type. The
collector therefore ships two hardcoded whitelists:

- **Entity types** (5): `cluster-domclient`, `host-domclient`, `disk-group`,
  `capacity-disk`, `cache-disk`. These are intersected with what
  `VsanPerfGetSupportedEntityTypes` reports for your environment, so unsupported
  types are never queried. An empty intersection logs a warning naming the whitelist.
- **Metric labels** (15): the IOPS, throughput, latency, congestion, outstanding-IO,
  disk-group capacity and cache-hit families. The whitelist is passed to vCenter as
  `VsanPerfQuerySpec.Labels`, so excluded metrics are never transferred, not fetched
  and discarded. Notably excluded are the 24 resync classification counters and the
  scheduler queue internals, which only matter during deep troubleshooting.

Neither list is configurable. If you need an entity type the API declines to report,
`-collector.vsan.perf.skip-verify` bypasses negotiation and queries the whole entity
whitelist directly.

**Aggregation semantics.** Every metric is treated as an instantaneous reading and
**averaged over the query window**. vSAN's own `iops_*` and `throughput_*` values are
already rates rather than cumulative counters, and latency is already an average, so
the window mean is the window's average level. Nothing is summed and nothing is
converted to a rate — do not wrap these in `rate()`.

Metric names are `vmware_vsan_perf_<label>`, with the entity type in the `entity`
label rather than in the name. That way `sum by (entity) (vmware_vsan_perf_iops_read)`
works in one line instead of requiring a join across metric names. The `entityid`
label carries the UUID vSAN reports for the entity; vSAN gives no friendly name, so
join on `vmware_cluster_info` via `cmo` for cluster context.

### Environment variables: mind the case

With `-envflag.enable`, a variable name is the `-envflag.prefix` value followed by
the flag name with dots replaced by underscores. **The flag name keeps its
original case** - it is not upper-cased. So with `-envflag.prefix=VMWARE_`:

| flag | variable |
| ---- | -------- |
| `-vmware.password` | `VMWARE_vmware_password` |
| `-vmware.vcenter` | `VMWARE_vmware_vcenter` |
| `-vmware.insecureTLS` | `VMWARE_vmware_insecureTLS` |
| `-http.address` | `VMWARE_http_address` |

`VMWARE_VMWARE_PASSWORD` is **silently ignored**. There is no warning and no
error - the exporter simply uses the flag default, and the only symptom is a
login failure with no explanation. `scripts/check_config.py` checks the names
used in `docker-compose.yml` against the flags the binary actually registers, so
a typo fails in CI rather than in production.

## The debug console

`/debug` serves an interactive page for testing a target before wiring it into
Prometheus. Fill in the address and credentials, tick the collectors you want,
hit **Run** and you get the raw exposition text back, plus the scrape duration
and which collectors succeeded. It is the fastest way to answer "are the
credentials right and does this account have the permissions the vSAN collector
needs" without editing `prometheus.yml` and waiting for a scrape interval.

The page submits to `/probe` over **POST**, with the parameters in the request
body rather than the query string. That is deliberate: a password in a query
string ends up in the browser's address bar and history, and in the access log of
every reverse proxy that logs query strings. In a request body it does not.

`Copy /probe URL` builds the equivalent GET URL for your `prometheus.yml`, with
the password replaced by a `<password>` placeholder — the URL is meant to be
pasted into a config file or a ticket, so it must not carry the real secret.

**The console is enabled by default. Consider turning it off** with
`-web.debug-console=false` on any listener that is reachable beyond your own
workstation. The form lets anyone who can load the page make the exporter open a
connection to an arbitrary address with arbitrary credentials — on an
unauthenticated port that is a credential probe with someone else's source IP.
With the flag off the route does not exist at all and returns 404; `/metrics`,
`/probe` and the landing page are unaffected. The same flag also removes
`/config`, since both are interactive pages rather than scrape endpoints.

The landing page at `/` lists every registered collector with its default state
and, where relevant, its cost (`per-host serial` for the esxcli collectors,
`needs vSAN` / `needs perf service` for the vSAN ones). That list is generated
from the collector registry, so it cannot drift from what the binary actually
supports.

## The configuration generator

`/config` turns a list of vCenters into the two files Prometheus needs: a
`scrape_configs` job and the file service discovery target list it reads. Paste
the addresses in — one per line, optionally with `address,username,password` —
pick the collectors, and copy or download the result.

Everything is generated in the browser. The page posts nothing back, so the
credentials you type never reach the exporter, and nothing is written to disk on
the server. Passwords are replaced with a `<password>` placeholder by default,
because the generated text is meant to be pasted into a ticket or a review.

**Why file service discovery** rather than `static_configs`: a static list lives
inside `prometheus.yml`, so adding a vCenter means editing the Prometheus
configuration and reloading it. A `file_sd_configs` target file is watched by
Prometheus and picked up on change — no reload, no restart. The file must end in
`.json`, `.yml` or `.yaml`, otherwise it is ignored silently.

Two target file styles are offered for the `/probe` mode:

| Style | Target file holds | Scrape config holds | Use when |
| :--- | :--- | :--- | :--- |
| **Addresses only** (default) | vCenter addresses and credentials | `relabel_configs` mapping them into request parameters | Normal case. The exporter address appears once, the file stays readable |
| **Parameters inlined** | Each entry's full `__param_*` set | Almost nothing | vCenters that need different schemes, TLS settings or collectors inside one job |

The inlined style has one trap the generator handles for you: `instance` must be
set explicitly. Prometheus only derives `instance` from `__address__`, which in
this mode is the exporter — so without it every vCenter reports under the same
instance and their series overwrite each other. Nothing errors; the data is just
wrong. Note also that several collectors cannot be inlined, because a label holds
a single value while `collect[]` needs to repeat; the generator falls back to a
job-wide `params` list in that case.

For the single-target `/metrics` mode the generator emits the exporter start-up
flags instead, with the credentials there rather than in any Prometheus file. The
input is still a list of vCenters, but each one becomes its own exporter process,
so the generated ports count up from the one in the form and the scrape targets
are those listen addresses — not the vCenters. Two processes cannot share a port;
the second exits with `address already in use`. Renumber the ports to match what
your configuration management allocates, keeping the target file and the command
lines in step.

Each target in that mode gets a `vcenter` label, because the exporter emits no
such label itself: without one the only thing telling two vCenters apart is an
`instance` like `localhost:9170`.

Validate the result before shipping it:

```bash
promtool check config prometheus.yml
```

## Securing the exporter

Two separate things are worth protecting, and they are easy to confuse:

1. **The connection to vCenter/ESXi.** Controlled by `-vmware.schema` and
   `-vmware.insecureTLS`. Defaults to HTTPS.
2. **The exporter's own listener** - the one Prometheus scrapes. Controlled by
   `-web.config.file`, and **unprotected by default**.

The second one matters more than it looks. The `/probe` endpoint accepts vCenter
credentials as URL query parameters, in a POST form body, or via HTTP basic auth,
so on a plain HTTP listener those credentials travel unencrypted, and the
query-parameter form also lands in the access logs of any reverse proxy in
between and in Prometheus's own logs. Prefer basic auth or a POST body over
`?password=`, and enable TLS.

> **`basic_auth_users` and `/probe` basic auth cannot both be used.** The
> exporter's own basic auth reads the same `Authorization` header that `/probe`
> reads vCenter credentials from, and it does not strip the header after
> validating it — whichever one is checked first wins, and the vCenter
> credentials never arrive. If you protect the listener with
> `basic_auth_users`, pass the vCenter credentials as parameters instead
> (query string for Prometheus, POST body for the debug console), and rely on
> TLS to keep them confidential.

Point `-web.config.file` at a file in
[exporter-toolkit format](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md):

```yaml
tls_server_config:
  cert_file: /etc/vmware-exporter/cert.pem
  key_file: /etc/vmware-exporter/key.pem

basic_auth_users:
  # bcrypt hash, e.g. from `htpasswd -nBC 12 "" | tr -d ':\n'`
  prometheus: $2y$12$hK1n...
```

```bash
./vmware-exporter -web.config.file=/etc/vmware-exporter/web-config.yml ...
```

### Keeping credentials out of process listings

A password passed as `-vmware.password=...` is visible to anyone who can read
`/proc` on the host, to `ps` inside a container, and to `docker inspect`. Pass it
through the environment instead:

```bash
docker run -d --name vmware-exporter -p 9169:9169 \
  -e VMWARE_vmware_username -e VMWARE_vmware_password -e VMWARE_vmware_vcenter \
  meisite/vmware-exporter:latest \
  -envflag.enable -envflag.prefix=VMWARE_ -vmware.insecureTLS
```

### systemd deployment

The release tarball ships the binary together with a `systemd/` directory that
contains the unit, a `config.yaml` template, and `install.sh` / `uninstall.sh`
helpers. Install with the helper (x86_64, systemd 232+):

```bash
tar xzf vmware-exporter-*-linux-amd64-systemd.tar.gz
cd vmware-exporter-*-linux-amd64-systemd
sudo ./install.sh
```

`install.sh` places the binary at `/usr/bin/vmware-exporter`, the unit under
`/etc/systemd/system/`, and a `config.yaml` template at
`/etc/vmware-exporter/config.yaml`, then enables (but does not start) the
service. Fill in your vCenter details:

```yaml
vmware.vcenter: vcenter.example.com:443
vmware.username: readonly@vsphere.local
vmware.password: "YOUR_PASSWORD"
vmware.insecureTLS: true
```

then start it:

```bash
sudo systemctl start vmware-exporter
```

Credentials are passed through the `-file` config rather than the command line,
so the password never lands in `/proc/<pid>/cmdline`. The config is a flat
`flag-name: value` mapping; full details, upgrades, uninstall and troubleshooting
live in `packaging/systemd/DEPLOY-zh.md`.

> **Note on permissions.** Unlike an `EnvironmentFile` (which systemd reads as
> root), `-file` is opened by the exporter itself after `DynamicUser=yes` takes
> effect, so `config.yaml` must be `0644 root:root`, not `0600`. `install.sh`
> corrects this on every run.

**`reload` applies configuration changes without dropping metrics.** The
exporter handles SIGHUP by re-reading `-file` and the environment variables and
writing the values back into the flags the request path reads. Every scrape
re-reads the collector switches and every login re-reads the `-vmware.*`
credentials, so the next scrape picks up the new configuration — no restart, no
gap in the time series.

```bash
sudo systemctl reload vmware-exporter
```

A failed reload **keeps the previous configuration** rather than leaving the
process half-configured, and reports itself through a metric:

```promql
# alert on this: `systemctl reload` still exits 0, because the signal was
# delivered successfully — only the metric and the log show the failure
vmware_exporter_config_last_reload_successful == 0
```

Three settings cannot be reloaded and still need a `restart`. Changing them is
reported in the log as a warning rather than applied silently:

| Flag | Why |
|------|-----|
| `-http.address` | the listener is already bound |
| `-web.config.file` | TLS and basic auth are loaded at listen time |
| `-log.format` | the log handler type is fixed at construction |

`-log.level` **is** reloadable, which is the most common reason to reload:
temporarily switch to `debug` without interrupting collection.

> Earlier versions installed no signal handler at all, so SIGHUP terminated the
> process and `systemctl reload` reported success while killing the service. If
> you are running a binary from before this change, use `restart`.
> `scripts/check_config.py` now checks the unit against the source in both
> directions so the two cannot drift apart again.

```bash
sudo systemctl restart vmware-exporter   # for the three flags above
sudo systemctl status vmware-exporter
journalctl -u vmware-exporter -f
curl -s localhost:9169/metrics | grep '^vmware_up'
```

<details>
<summary>systemd < 232 (CentOS 7, etc.)</summary>

The unit uses `DynamicUser=yes`, which requires systemd 232+. On older systems
create a real account instead:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin vmware-exporter
sudo sed -i 's/^DynamicUser=yes/User=vmware-exporter\nGroup=vmware-exporter/' \
  /etc/systemd/system/vmware-exporter.service
sudo systemctl daemon-reload && sudo systemctl restart vmware-exporter
```

Some hardening directives (`ProtectKernelLogs`, `ProtectClock`,
`RestrictSUIDSGID`, etc.) may be unknown to older systemd versions — they
produce warnings but are safely ignored. Run `systemd-analyze verify` to
confirm.

</details>

Use a **read-only** vCenter service account. The exporter only reads properties
and performance counters; it never writes.


## Self-monitoring

Every scrape emits health metrics, so a collector that silently fails is visible
without reading logs:

```
vmware_up 1
vmware_scrape_duration_seconds 1.512
vmware_scrape_collector_duration_seconds{collector="host"} 1.284
vmware_scrape_collector_success{collector="host"} 1
vmware_scrape_errors_total{collector="host"} 0
vmware_scrape_errors_total{collector="login"} 0
```

| Metric | Type | What it answers |
| --- | --- | --- |
| `vmware_up` | gauge | Could the target be logged into at all? `0` means no inventory data was produced. Not the same as Prometheus's built-in `up`, which only reports whether the HTTP request succeeded — and *HTTP fine, vCenter login rejected* is a routine outcome for a multi-target exporter |
| `vmware_scrape_duration_seconds` | gauge | Total scrape time, login and logout included |
| `vmware_scrape_collector_duration_seconds{collector}` | gauge | Per-collector time. `collector="login"` covers authentication |
| `vmware_scrape_collector_success{collector}` | gauge | Did the *last* scrape of this collector work? |
| `vmware_scrape_errors_total{collector}` | **counter** | How many times has it failed? `collector="login"` covers authentication failures |
| `vmware_exporter_build_info` | gauge | Which build is running (`version`, `revision`, `branch`, `goversion`) |

`success` and `errors_total` answer different questions, and you want both.
`success` is a snapshot — it cannot tell you whether a collector failed twelve
times in the last hour and happened to recover just before the last scrape. A
vCenter that times out intermittently reads as flicker on the gauge, and anything
that fails and recovers between two samples is invisible to it. The counter
cannot miss it.

```promql
# target unreachable or credentials rejected
vmware_up == 0

# one collector failing while the rest of the scrape succeeds
min_over_time(vmware_scrape_collector_success[5m]) == 0

# sustained failure of a single collector
increase(vmware_scrape_errors_total{collector!="login"}[15m]) > 3

# credentials rejected
increase(vmware_scrape_errors_total{collector="login"}[15m]) > 0
```

Collectors that have never failed report `0` rather than being omitted. That is
deliberate: a series that does not exist while everything is healthy makes the
alert above read *no data* instead of *zero*, and once the first failure creates
the series, `increase()` has no prior sample to compute a delta from — so the
first outage would be the one you miss.

Login failures are counted under `collector="login"` only, never spread across
the individual collectors. One expired password would otherwise read as N+1
separate failures and bury the signal you want.

In `/probe` mode the counts are bucketed per target, so a rejected password on
one vCenter cannot inflate the error count of another.

`vmware_exporter_build_info` only carries real values if the binary was built
with the version linker flags — the release builds and the `Dockerfile` set them
(see `--build-arg` in the Dockerfile), a plain `go build` does not.

Unknown collector names passed via `collect[]` are rejected with HTTP 400 rather
than silently ignored.

The esxcli collectors are a very specific use case that probably is not going to be needed by anyone. Left the code in here as an example on how custom information can be collected using esxcli command tool remotely via the SOAP API (`vim.EsxCLI.*`) — no SSH involved. 

## The full metric reference

The metrics above are only the exporter's own health signals. **[`docs/METRICS.md`](docs/METRICS.md)
is the complete reference** ([中文版](docs/METRICS-zh.md)) — every metric this
exporter can emit, the exact label set it carries, and what the value actually
means, including the naming rules the vSphere performance counters go through
before they reach Prometheus.

That document is part of the contract, not commentary. Adding, renaming or
removing a metric or a label means updating **both language versions in the same
change** — and this is enforced rather than remembered: `scripts/check_config.py`
parses the metric declarations out of the Go sources and compares them against
the tables in each document, in both directions. A metric declared in the code
but missing from a document fails the check, and so does an entry left in a
document after the metric was renamed or dropped. Checking only the English file
would let the translation rot, so both are held to the same contract.

```bash
python3 scripts/check_config.py
```

## Metric changes and migration

This release normalises **every** metric name to Prometheus conventions: base
units in the name, a unit suffix, `_total` on counters, and no vSphere rollup
suffixes. `CHANGELOG.md` has the complete table; this section is the operational
summary.

### The bundled dashboards are already migrated

The Grafana dashboards in `dashboards/` use the new metric names, so they work
against a default exporter with no extra flags. They were migrated by
`scripts/migrate_dashboards.py`, which is kept in the repository so the change is
reproducible and reviewable rather than a one-off hand edit.

Renaming the queries was not enough on its own. Two other things had to change
with them, and both are silent failures if missed:

- **Unit conversions moved into the exporter.** Panels used to multiply by
  `1024`, `1000 * 1000` or `8192` to turn the exporter's kiloBytes and MHz into
  bytes and hertz. The exporter now emits base units, so those factors were
  removed and the panel units updated (`kbytes` → `bytes`, `KiBs` → `Bps`,
  `ms` → `s`). Leaving a factor in place would have rendered a number wrong by
  three orders of magnitude, with nothing to indicate it.
- **The CPU ready/costop panels were rewritten to use `rate()`.** They divided a
  summation counter by a hardcoded `20 * 1000`, which assumed a 20-second vSphere
  granularity. Those panels now use `rate(..._seconds_total[$__rate_interval])`,
  which derives the window from the query step instead of assuming it.

Two pre-existing dashboard bugs were fixed in the same pass — see `CHANGELOG.md`.

`scripts/check_config.py` verifies on every CI run that no panel references a
pre-rename metric or reapplies a conversion the exporter now performs itself.

If you have **your own** dashboards or alerting rules on the old names, either
run with `-metrics.legacy` while you migrate them, or use the recording rules
below.

### Metric naming

Names are now derived from the counter metadata vCenter itself reports, not from
a hand-maintained list. Three rules cover almost everything:

| Rule | Example |
| --- | --- |
| The vSphere rollup suffix (`.average`, `.summation`, `.latest`) is dropped — it describes how vCenter aggregates, not what the value is | `cpu.usagemhz.average` → `cpu_usage_hertz` |
| The unit becomes a suffix, converted to a Prometheus base unit | `mem.consumed.average` (kiloBytes) → `mem_consumed_bytes`, value ×1024 |
| Counters vCenter declares as `delta` become real counters with `_total` | `cpu.ready.summation` → `cpu_ready_seconds_total` |

Two conversions are worth calling out because getting them wrong still produces
a plausible-looking number:

- **`percent` counters are divided by 10000, not 100.** vSphere reports percent
  in hundredths of a percentage point — a raw value of `100` means 1%. The new
  `*_ratio` metrics are in the 0..1 range Prometheus expects, so a panel showing
  them needs unit `percentunit`, not `percent`.
- **`kiloBytes` is 1024 bytes, `megaBytes` is 1048576.** vSphere documents these
  as binary multiples. Using 1000 would understate memory by 2.4%.

### Values changed, not just names

Unlike the previous release's deprecations, several replacements carry a
different number. Anything comparing a metric against a hardcoded threshold
needs the threshold rescaled.

| Legacy name | Replacement | Multiply legacy by |
| --- | --- | --- |
| `vmware_host_cpu_capacity`, `vmware_host_cpu_capacity_mhz` | `vmware_host_cpu_capacity_hertz` | 1000000 |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | 1 |
| `vmware_vm_mem_capacity` | `vmware_vm_mem_capacity_bytes` | 1048576 |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | 1 |
| `vmware_datastore_capacity` | `vmware_datastore_capacity_bytes` | 1 |
| `vmware_datastore_free` | `vmware_datastore_free_bytes` | 1 |

`vmware_host_cpu_capacity_mhz` was introduced by the previous release as the
replacement for `vmware_host_cpu_capacity`. It is itself deprecated now: MHz is
not a Prometheus base unit. If you already migrated to `_mhz`, migrating again
is a multiplication by 1000000.

### Delta counters: stop dividing by the sample interval

The old `*_summation` metrics were gauges holding the mean of the samples in the
scrape window, and the idiomatic way to turn one into a rate was to divide by a
hardcoded interval:

```promql
# old — the 20 is -vmware.granularity, hardcoded into the query
vmware_host_cpu_ready_summation / (20 * 1000)
```

That expression is wrong as soon as `-vmware.granularity` is not 20, and it was
also wrong on the exporter side: averaging delta samples discards all but one
interval's worth of increments. Both halves are fixed. The replacement is a
counter, so use `rate()` and let Prometheus work out the interval:

```promql
# new — no hardcoded interval, correct for any granularity
rate(vmware_host_cpu_ready_seconds_total[$__rate_interval])
```

This applies to `cpu_ready`, `cpu_costop`, `cpu_maxlimited` and the four
`net_*_errors_total` / `net_*_dropped_total` counters.

### Keeping old names working with recording rules

If you would rather not touch your dashboards at all, recording rules can
reconstruct the old names from the new ones. This is a better long-term position
than `-metrics.legacy` because the aliases live in your Prometheus config, where
you can delete them one at a time:

```yaml
groups:
  - name: vmware-exporter-legacy-aliases
    rules:
      - record: vmware_host_cpu_capacity
        expr: vmware_host_cpu_capacity_hertz / 1000000
      - record: vmware_host_mem_capacity
        expr: vmware_host_mem_capacity_bytes
      - record: vmware_vm_mem_capacity
        expr: vmware_vm_mem_capacity_bytes / 1048576
      - record: vmware_datastore_capacity
        expr: vmware_datastore_capacity_bytes
      - record: vmware_datastore_free
        expr: vmware_datastore_free_bytes
```

Note that a recording rule cannot reproduce the old `*_summation` gauges
faithfully — their old values were wrong whenever more than one sample fell in
the scrape window. Migrate those to `rate()` rather than aliasing them.

### Renames from the previous release

| Change | Action |
| --- | --- |
| `vmware_cluster_datastores` → `vmware_cluster_datastore` | Update your own rules/panels. Also emits one series per datastore now, instead of a comma-joined list in `dsmo` |
| `vmware_compute_datastores` → `vmware_compute_datastore` | Same as above |
| `vmware_vm_snapshot_info` lost its `created` label | Read the creation time from the metric value — it is the same instant as a Unix timestamp |
