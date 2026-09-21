# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ✨ Added

- **Lifecycle visibility for powered-off VMs and maintenance hosts
  (Batch 0 of the P1/P2/P3 roadmap, the v0.2.0 breaking change).** The
  exporter used to drop a VM the moment it was powered off and a host the
  moment it disconnected or entered maintenance mode — `_info` and capacity
  metrics vanished along with the performance counters, so Prometheus could
  not tell "the object was deleted" from "the object exists but is down", and
  every capacity dashboard silently lost its denominator during a maintenance
  window. The inventory surface (`_info`, capacity, status) is now emitted for
  **all** entities; only the perf data plane skips entities vCenter holds no
  real-time samples for.

  New metrics, all gauges:

  | Metric | Labels | Purpose |
  | --- | --- | --- |
  | `vmware_vm_power_state` | `vmmo`, `vm`, `state`, `vcenter` | State label (`poweredOn`/`poweredOff`/`suspended`/`unknown`), value 1 |
  | `vmware_host_power_state` | `hostmo`, `host`, `state`, `vcenter` | `poweredOn`/`poweredOff`/`standBy`/`unknown`, value 1 |
  | `vmware_host_connection_state` | `hostmo`, `host`, `state`, `vcenter` | `connected`/`disconnected`/`notResponding`, value 1 |
  | `vmware_host_maintenance_mode` | `hostmo`, `host`, `vcenter` | Plain 0/1 boolean |
  | `vmware_vm_overall_status` | `vmmo`, `vm`, `status`, `vcenter` | `green`/`yellow`/`red`/`gray`, value 1 |
  | `vmware_host_overall_status` | `hostmo`, `host`, `status`, `vcenter` | same four colours |
  | `vmware_datastore_overall_status` | `dsmo`, `ds`, `status`, `vcenter` | same four colours |
  | `vmware_cluster_overall_status` | `cmo`, `vmwcluster`, `status`, `vcenter` | same four colours |
  | `vmware_cluster_effective_hosts` | `cmo`, `vmwcluster`, `vcenter` | Connected, powered-on, non-maintenance hosts |
  | `vmware_cluster_cpu_capacity_hertz` | `cmo`, `vmwcluster`, `vcenter` | `summary.totalCpu` MHz × 1e6 |
  | `vmware_cluster_cpu_effective_hertz` | `cmo`, `vmwcluster`, `vcenter` | `summary.effectiveCpu` MHz × 1e6, after HA reservations |
  | `vmware_cluster_memory_capacity_bytes` | `cmo`, `vmwcluster`, `vcenter` | `summary.totalMemory`, already bytes |
  | `vmware_cluster_memory_effective_bytes` | `cmo`, `vmwcluster`, `vcenter` | `summary.effectiveMemory` (MB) × 1048576 |
  | `vmware_scrape_entities_found` | `vcenter`, `collector`, `kind` | Entities discovered in the last scrape |
  | `vmware_scrape_entities_emitted` | `vcenter`, `collector`, `kind` | Entities that got data-plane metrics |
  | `vmware_scrape_entities_skipped` | `vcenter`, `collector`, `kind`, `reason` | Per-reason skip count for the last scrape |

  Notes that matter operationally:

  - The state/overall-status shape mirrors the existing
    `vmware_resourcepool_overall_status`: status in a label, constant value 1,
    one series per state — no scatter of one boolean metric per possible state.
    `gray` (unknown) is always emitted verbatim; it frequently precedes a real
    failure and must not be folded into `yellow`.
  - The three `vmware_scrape_entities_*` metrics are **per-scrape snapshot
    gauges**, not counters — they deliberately have no `_total` suffix
    (promlint forbids it on non-counters), so alert on the raw value or on
    `found - emitted`, never on `rate()`. Every reason a collector can produce
    is pre-filled with `0`, keeping the series set stable so alerts need no
    `absent()`/`or`; one entity can match several reasons (maintenance *and*
    disconnected), so cross-reason sums can exceed the skipped count.
  - `vmware_cluster_effective_hosts` lost the draft's `_count` suffix for the
    same promlint rule (`_count` is reserved for histograms/summaries).
  - The new `uuid` label on `vmware_vm_info` comes from
    `summary.config.uuid` and on `vmware_host_info` from
    `hardware.systemInfo.uuid` — both already in the fetched property sets, so
    the change adds **zero** API round trips. It is an extra label only;
    `moid` + `vcenter` remain the join key. Datastore gets no separate uuid
    label: pulling the large `info` property for one label is not worth it, and
    the VMFS volume UUID already rides on the existing `pfinstance` label.
  - Disconnected hosts legitimately have no hardware/product summary; their
    hardware/software/capacity metrics are skipped while `_info` and the state
    metrics still report them.
  - **The bundled dashboards were replaced and made lifecycle-aware.** The five
    legacy `vmware-{vcenter,cluster,host,vm,datastore}-view.json` dashboards are
    superseded by a new hand-authored, VictoriaMetrics-oriented set:
    `vmware-{vcenter,cluster,host,datastore}-overview.json` and
    `vmware-vm-detail.json` (a `victoriametrics-metrics-datasource`, `$job`/
    `$target` variables, `topk_avg`, tuple aliases). Without action, the new
    estate panels would have silently drifted: "running VMs" would count
    powered-off VMs and every host utilisation/overcommit denominator would
    stay large through a maintenance window (perf numerators still skip the
    host). Every estate/cluster aggregate therefore intersects the static
    series with an explicit eligible set (`power_state{poweredOn}` +
    `connection_state{connected}` + `maintenance_mode == 0` for hosts,
    `power_state{poweredOn}` for VMs), keeping the pre-v0.2.0 numbers; panels
    driven by a real-time perf `* on(...) group_left info` inner join need no
    change because vCenter already omits the ineligible entities. Single-entity
    drill-downs (vm-detail, host-overview) intentionally keep showing
    configured capacity while the host/VM is off — visibility is the point
    there. Each dashboard additionally gets a **lifecycle & health row** using
    the new metrics: powered-off/suspended VM counts, maintenance/disconnected/
    not-responding hosts, non-green `overall_status` entities, cluster total-vs
    -effective CPU/memory capacity, and the scrape
    found/emitted/skipped entity gauges. The policy (which panels are wrapped,
    kept as inventory, or treated as perf joins) is keyed by file + panel id in
    `scripts/lifecycle_dashboards.py`, with a `--check` audit wired into
    `check_config.py`: an estate panel of the old unfiltered shape, a drifted
    override, or a missing lifecycle row fails the build. Every changed/new
    expression parses under the upstream PromQL parser (Grafana `$variables`
    substituted; the dashboards' own VictoriaMetrics-only tuple/alias syntax is
    valid in VictoriaMetrics by construction).

  - See the migration guide under **Breaking changes** below; the new metric
    contract is registered in `docs/METRICS.md` / `docs/METRICS-zh.md` and the
    round-trip `check_config.py` guard is green.

- **Chunked performance queries for large vCenter estates
  (`-vmware.perf.chunk-size`, default 64).** Host, VM and datastore performance
  collection used to put every entity into a single `QueryPerf` SOAP request.
  vCenter enforces `vpxd.stats.maxQueryMetrics` (entities × counters); with a
  few thousand VMs and a dozen counters the single request either failed
  outright or exceeded `-vmware.timeout`, losing that whole counter group.
  Entities are now split into bounded chunks queried with bounded concurrency
  (the width is `-collector.max-concurrency`, still capped by the global SOAP
  throttle), then merged back in entity order. Counter metadata is resolved
  once instead of once per chunk (govmomi's `SampleByName` re-issues
  `CounterInfoByName` on every call), and the historical tail-truncation is
  reproduced so datastore 300s rollup values are identical to the old path.
  Set the flag to `0` for the pre-v0.1.20 single-request behaviour.

- **Process-level TTL cache for slow-changing inventory
  (`-scrape.inventory-cache-ttl`, default 5m).** Every enabled collector
  previously issued a `CreateContainerView` + `RetrieveProperties` + `Destroy`
  round trip on every 20s scrape, even for topology that changes rarely.
  Datacenter, folder, cluster, standalone compute resource, datastore, resource
  pool and the vSAN cluster-name discovery now share a per-target cache keyed
  by object types **and** requested properties (so a smaller property set can
  never satisfy a larger one); concurrent misses collapse through
  singleflight, failed retrievals are never cached, and expired entries are
  evicted. Host/VM runtime state — which decides power/maintenance filtering
  for perf queries — and every performance counter stay strictly real-time.
  The cache is injected **only** on the single-credential `/metrics` path; the
  multi-tenant `/probe` path always passes a nil cache so one set of
  credentials can never read another's inventory. Set the flag to `0` to
  disable.

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

- **Hardening and throughput flags for the scrape surface.**
  A security/performance re-audit added three knobs plus an unconditional HTTP
  timeout fix:

  - `-web.max-scrape-inflight` (default `4`, `0` disables) caps how many
    `/metrics` and `/probe` scrapes run at the *same time*. The existing
    `-collector.max-concurrency` only bounds goroutines *inside one* scrape;
    overlapping scrapes each logged in separately and fanned out independently,
    so scrape intervals shorter than one scrape duration could pile logins onto
    vCenter and exhaust its session table. Over-limit requests now get a fast
    **HTTP 503** instead of queueing. The lightweight `-disable.exporter.target`
    path is not gated (it never logs in).

  - `-probe.allowed-targets` (default empty = allow any, unchanged) is an
    optional `/probe` target allowlist as defence-in-depth against SSRF: rules
    are comma-separated and match the target *host* as a suffix starting with
    `.` (`.example.com`), a CIDR (`10.0.0.0/8`), or an exact host/IP. A target
    outside the list gets **HTTP 403** before any credentials are used.

  - The HTTP server now sets `ReadHeaderTimeout` (5s) and `IdleTimeout` (60s).
    exporter-toolkit sets neither, so the previous bare `http.Server{}` waited
    indefinitely for request headers — a Slowloris client could hold a
    connection and goroutine for free. `WriteTimeout`/`ReadTimeout` are
    deliberately left unset because a legitimate scrape can run the full
    `-vmware.timeout`.

  - The esxcli per-host × per-NIC fan-out is now bounded **globally per
    scrape**, not just per errgroup layer. The vim25 client's `RoundTripper` is
    wrapped after login so the token is held only for one in-flight SOAP call;
    nested goroutines queue at the gate without holding a token while waiting
    on children, so the worst case is `max-concurrency` concurrent SOAP calls
    instead of `n × n`. vSAN uses a separate service client and stays bounded
    by its own (non-per-host) call pattern.

  Each of these is reverse-verified: a test fails when the gate always admits,
  when the allowlist never rejects, and when the SOAP wrapper bypasses its
  semaphore (observed peak then equals the unthrottled total).

### 🔐 Security — /probe hardening (S1)

- **SSRF allowlist userinfo bypass closed (CVE-class, high).** The allowlist
  and the SOAP client parsed the `target` parameter differently:
  `allowed.example.com:443@169.254.169.254` matched the `.example.com` suffix
  rule via `net.SplitHostPort` while the URL/SOAP layer actually connected to
  the host after `@` — and the request's Basic credentials were forwarded to
  that host. Both the HTTP layer and the connection layer now normalize the
  target through one parser (`internal/target`), which rejects userinfo, path,
  query and fragment and accepts only `host` / `host:port`. The vcenter label
  and internal error buckets now use the normalized authority, so case/whitespace
  variants no longer create separate buckets.
- **Unbounded `/probe` target memory growth closed.** `vmware_scrape_errors`
  is keyed by target, and on `/probe` the target (and the login-failure count)
  comes from the request; an unauthenticated caller could grow the map without
  limit by sending a fresh random target each scrape. Distinct target buckets
  are now bounded by an LRU (default 1000, far above any real fleet size) and
  targets are normalized before bucketing.
- **`/probe` request body is capped at 1 MiB** (`http.MaxBytesReader`, 413 on
  overflow). Go reads an entire `application/x-www-form-urlencoded` body into
  memory; without a cap an oversized POST was an unauthenticated memory DoS.
  GET query strings are unaffected.
- **`schema` parameter restricted to `http`/`https`** (400 otherwise), so a
  request cannot force an arbitrary scheme. Legitimate inputs are unchanged.

### 🔐 Security — deployment hardening (S2: S-03, S-04)

- **systemd config containing the vCenter password is no longer world-readable
  (S-03).** The unit previously ran under `DynamicUser=yes`, whose ephemeral uid
  cannot read a root-owned `0600` file, so `install.sh` had to ship `config.yaml`
  as `0644` — any local user on the host could `cat` the vCenter credentials.
  The unit now runs as a static, non-login system account
  (`User=`/`Group=vmware-exporter`), which `install.sh` creates idempotently,
  and the config is installed `vmware-exporter:vmware-exporter 0600`; upgrades
  tighten a legacy `0644` file back to `0600`. The static account also keeps
  `-file` pointing at the real file, so SIGHUP hot-reload still works (a
  `LoadCredential=` design was evaluated and rejected: it hands the process a
  boot-time read-only snapshot, which would silently break reload and needs
  systemd 248+). The DynamicUser/systemd-232 requirement is gone; standard
  `User=` works on any supported systemd. DEPLOY-zh.md, both READMEs and the
  config template were updated.
- **Container image runs as an unprivileged user (S-04).** The `scratch` image
  now ships a minimal `/etc/passwd` for uid/gid `65534` (nobody) and declares
  `USER 65534:65534` — it previously ran as root. The binary is static, listens
  on an unprivileged port and writes nothing to disk (assets are go:embed-ed),
  so this is drop-in. `docker-compose.yml` is hardened to match the systemd unit:
  loopback-only bind (`127.0.0.1:9169`), `read_only: true`, `cap_drop: [ALL]`
  and `no-new-privileges:true`; the README docker examples were switched to
  env-flag credentials (out of `docker inspect`/`ps`) with the same flags.
  Expose it across hosts only behind a TLS+auth reverse proxy.

### 🔐 Security — defence in depth (S3 partial: S-08, M-02, M-04)

- **Security response headers on every reply (S-08).** All HTTP responses now
  carry `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`
  via a wrapper around the mux (covers `/metrics`, `/probe`, `/debug`, `/config`
  and the landing page). These matter when the interactive pages are opened in
  a browser.
- **Startup warning for unauthenticated non-loopback exposure (S-08).** When
  `-http.address` binds beyond loopback (e.g. `:9169` / `0.0.0.0`) **and**
  `-web.config.file` is unset (no TLS/Basic auth), the exporter logs a prominent
  warning that `/probe` credentials travel in clear text and the port is
  reachable without authentication. It is advisory only and does not stop
  startup (a loopback bind behind a reverse proxy remains the common valid case).
  Loopback (`127.0.0.0/8`, `::1`, `localhost`) and any deployment that supplies
  a web config stay silent.
- **Inventory cache is capacity-bounded (M-02).** `/metrics` normally holds a
  tiny, finite set of keys (managed-object type set × property set), but the
  cache previously had no hard ceiling and only removed expired entries when the
  same key was revisited, so a never-revisited stale key lingered. There is now
  a default cap (4096, far above normal key counts): a `put` first reaps each
  entry past its own TTL, then evicts the oldest entries if the cap is exceeded.
  TTL/singleflight semantics are unchanged and `/probe` still never receives the
  cache.
- **esxcli XML escaping verified by test (M-04).** esxcli parameter values/names
  are placed into the SOAP `val` element, which is a normal `encoding/xml`
  content field, so `<`, `>` and `&` in a value are entity-escaped at
  serialisation and cannot inject live XML elements. This was previously an
  implicit assumption; it is now an explicit regression test (injection probe
  round-trips back to the exact original string). No production change was
  needed.

### 🔐 Security — outbound SSRF dial guard (S4: S-06)

- **The vCenter connection is now checked at the socket against DNS rebinding
  and metadata-endpoint SSRF (S-06).** The allowlist (`-probe.allowed-targets`)
  and the shared target parser only compare strings/parse the URL; an attacker
  who controls a whitelisted name's DNS can still return different addresses to
  the validation lookup and the real connection (TOCTOU), or a name can resolve
  to a link-local address the string check never sees. A new internal `safedial`
  package installs a `net.Dialer.Control` hook on the govmomi SOAP transport that
  runs after Go has resolved the name but immediately before `connect(2)`, so the
  IP checked and the IP connected are literally the same one — there is no
  rebinding window. **Both** dial paths are covered, which matters because
  govmomi sets its own `http.Transport.DialTLSContext` for HTTPS (vCenter is
  effectively always HTTPS) and that implementation calls `tls.Dial` directly,
  bypassing `DialContext`; a DialContext-only guard would never have fired in
  production. The replacement dials the guarded TCP connection first and then
  performs the TLS handshake on that exact connection.
  - **Default policy (zero false positives):** always reject link-local
    destinations — including the cloud metadata endpoint `169.254.169.254` —
    and the unspecified addresses `0.0.0.0`/`::`. Loopback and RFC1918/ULA
    private ranges are still allowed, because vCenter/ESXi overwhelmingly runs
    on the LAN and vcsim/`127.0.0.1` sidecar proxies use loopback.
  - **New opt-in flag `-vmware.deny-private-addresses`** (default `false`): when
    `true`, also reject loopback and private ranges. Enable only when the
    exporter reaches vCenter over routable addresses; enabling it for an on-LAN
    vCenter or a local sidecar proxy makes every login fail. The flag is part of
    the per-scrape settings snapshot and hot-reloads on SIGHUP.
  - The login error is now wrapped with `%w` (was `%s`), so callers can
    `errors.Is(err, safedial.ErrBlockedAddress)`; the rendered error text is
    unchanged.
  - Coverage: `internal/safedial` unit tests for every range boundary, the
    Control callback and both guarded dial paths (including an end-to-end HTTPS
    handshake through the guarded transport), plus two govmomi/vcsim login tests
    proving a loopback vcsim login succeeds by default and is rejected with the
    sentinel under the strict policy.

### 🔐 Security — query-string credentials opt-out (S4: S-07)

- **New opt-in flag `-probe.deny-query-credentials` (default `false`).** `/probe`
  has always accepted `?username=&password=` in the URL, the style the bundled
  Prometheus scrape configs use; the cost is that those credentials land in
  exporter and reverse-proxy access logs, the `Referer` header, browser history
  and tracing spans. With this flag enabled, any `/probe` request carrying
  `username` or `password` in the URL query string is rejected with 400 before
  the target is even parsed; credentials must then come from the POST form body
  or HTTP Basic Auth. The decision inspects only the query string and is
  independent of HTTP method, so a POST that still embeds credentials in the URL
  is rejected as well. Default `false` keeps the legacy GET-with-credentials
  behaviour byte-for-byte; the flag is read through the per-request settings
  snapshot and hot-reloads on SIGHUP. Tests cover rejection (full and partial
  query credentials, GET and POST), the two allowed channels (POST body, Basic
  Auth) and the default-compatible GET path.

### 🔐 Security — core dumps disabled so heap credentials never hit disk

- **Shipped deployments now forbid core dumps.** The process holds vCenter
  credentials in heap memory (the `-file` service password, per-request `/probe`
  credentials, and the post-login session cookie); a crash core dump would write
  that plaintext to disk regardless of the unprivileged run account, since cores
  are centrally collected by `systemd-coredump` under
  `/var/lib/systemd/coredump/`. The systemd unit now sets `LimitCORE=0`
  explicitly (previously it relied on the host default and only carried a comment
  about the old `LimitCORE=infinity` hazard), and `docker-compose.yml` sets
  `ulimits: core: { soft: 0, hard: 0 }` (use `--ulimit core=0:0` with
  `docker run`). Debugging is unaffected: Go `panic`/`SIGQUIT` stacks still go to
  stderr (journal/container logs), and the static CGO-free binary carries no
  local symbols a core would add. DEPLOY-zh.md and both READMEs document the
  rationale, how to verify (`systemctl show -p LimitCORE`), and the temporary
  `systemctl edit` override for capturing a one-off core.

### ⚡ Performance — scrape CPU/memory batch A

- **Default log level is now `info` instead of `debug`.** At `debug` every
  scrape assembled and (in json mode) serialized log arguments for each chunk,
  each skipped entity and each unavailable counter — measurable CPU and
  journal/IO cost in large environments, and the binary/container shipped with
  that default (the systemd template already pinned `info`). Verbose per-scrape
  logging is one `-log.level=debug` flag or a SIGHUP reload away; the flag
  continues to hot-reload, so no restart is needed for temporary debugging.
- **esxcli `host.nic` and `storage.core` adapter descriptors are reused.**
  Each esxcli metric series built a fresh `*prometheus.Desc` per entity per
  scrape; the entity identifiers (`moid`/`host`/device) are now variable labels
  and the descriptive hardware values (driver/version/firmware and
  vendor/model/revision) remain constant labels, so one Desc per metric-name +
  hardware-combination is cached and shared across all hosts and all scrapes.
  Exposed series, labels and cardinality are unchanged.
- **Datastore path-cleanup regexp compiled once** at package level instead of
  being recompiled on every datastore scrape.

### 📊 Observability — SOAP transport metrics (batch B, P-09)

- Three new self-monitoring series make the SOAP load on vCenter measurable,
  which until now had no instrumentation at all:
  - `vmware_soap_requests_total{vcenter,result}` — **counter**, total round
    trips accumulated across scrapes, split into `ok`/`error`. Requests that
    never acquired the concurrency token are not counted.
  - `vmware_soap_inflight{vcenter}` — **gauge**, peak round trips in flight
    during the *last* scrape (the instantaneous value is always 0 at scrape
    time; the peak is bounded by `-collector.max-concurrency` when set).
  - `vmware_soap_throttle_wait_seconds{vcenter}` — **histogram** of time spent
    queued for the SOAP concurrency token (buckets 1ms–10s), populated only
    when `-collector.max-concurrency` is greater than zero.
  Counts and the wait histogram are process-level and accumulate per target
  (shared across `/metrics` and `/probe`), so `rate()` works; the distinct
  target set is LRU-bounded (1000) with the same normalization as the scrape
  error counter, so a `/probe` caller cannot grow the map without limit.
  Instrumentation is now installed on the SOAP transport at *any* concurrency
  setting — at `-collector.max-concurrency=0` it is a pure pass-through wrapper
  that still counts, with no semaphore.

### ⚡ Performance — scrape CPU/memory batch C (P-03, P-08b)

- **Counter metadata is now cached on `/metrics` (P-03).** Every scrape builds
  a fresh `performance.Manager`, so govmomi's own *per-Manager* counter cache
  never survives a login — without this change each scrape paid a SOAP round
  trip plus a full parse of the `perfCounter` table and a by-name map rebuild on
  login. A new process-level `CounterCache` (singleflight-coalesced misses,
  failures not cached, TTL supplied per lookup so SIGHUP reloads apply at once)
  is keyed by target plus the vCenter About version/build; an upgrade is picked
  up within one TTL. New flag `-scrape.counter-cache-ttl` (default `10m`, `0`
  restores fetch-on-every-login). Like the inventory cache it is injected on
  `/metrics` only and stays live on the multi-tenant `/probe` path. Measured on
  a vcsim-sized table (560 counters) the login step goes from ~48 ms / 10 MiB /
  ~204k allocations to a ~0.2 µs in-process map read; real vCenter saves the
  cross-network round trip on top. Numbers and the methodology note (why a
  reused-Manager benchmark is misleading) are in
  `docs/perf/p03-counter-cache.txt`.
- **Perf result-set memory hygiene (P-08b, conservative).** `rawSeries` is now
  pre-sized to the sum of chunk lengths instead of growing via append, the
  duplicate `parts` reference is dropped right after merging, and the raw SOAP
  wrapper structs (`PerfEntityMetric`/`PerfMetricIntSeries`) are released as
  soon as `ToMetricSeries` returns (the returned series only shares the int64
  sample/SampleInfo backing arrays). This shrinks live-set during the long emit
  phase; a controlled benchmark shows total allocs/B/op are unchanged within
  noise (~45.4 MiB / ~848k allocs for 500 powered-on VMs) — the dominant cost is
  govmomi SOAP XML decode plus `ToMetricSeries`, which only the higher-risk
  streaming parse (P-08b-1, a later batch) can reduce. Metric output is
  byte-identical (full test suite green); see
  `docs/perf/p08b-perf-resultset.txt`.

### ⚡ Performance — batch D evaluated, deliberately not implemented

- **Streaming/chunk-local perf processing (P-08b-1) was benchmarked and parked.**
  The batch's other half, P-08a (skip non-powered-on VMs for the perf data
  plane), had already shipped. For P-08b-1 a vcsim 500-VM A/B test compared the
  current "collect every chunk → merge → one `ToMetricSeries` → emit" path
  against a chunk-local variant with no cross-chunk `rawSeries` aggregate: under
  normal GC the peak `HeapInuse` (~21–24 MiB), GC count (2/scrape) and total
  alloc (~44 MiB) all overlap within sampling noise. A GC-off run confirms the
  ~44 MiB is per-scrape transient garbage (govmomi's reflective SOAP XML decode
  inside `Query`) with ~zero retained heap after GC — not a leak. The only lever
  that cuts the total, a hand-written streaming XML decoder, is high-risk for no
  steady-state gain, so it is deferred until real large-fleet pressure
  (2000+ VMs approaching the memory limit / GC CPU) justifies it; restart
  criteria and the gate (separate branch, per-metric golden compare, `-race`,
  before/after numbers) are recorded in
  `docs/perf/p08b1-streaming-decision.txt`. No production code changed.

### ⚡ Performance — sampling-interval negotiation cached (per-entity-type)

- **`QueryPerfProviderSummary` is now cached on `/metrics`.** host, VM and
  datastore each negotiate the sampling interval once per scrape, and govmomi's
  `performance.Manager.ProviderSummary` is documented as "caching the value
  based on entity.Type" but actually issues a SOAP call every time (the Manager
  is rebuilt on every login, so even a real govmomi cache would not help across
  logins). A new process-level cache stores the raw
  `*types.PerfProviderSummary` keyed by target + vCenter About version/build +
  entity type; the negotiation and the ESXi force-realtime correction still run
  every scrape, only the round-trip is cached (each entity type is fetched once
  per TTL). Same boundary as the other caches: injected on `/metrics` only,
  never on the multi-tenant `/probe` path; failures are not cached, concurrent
  misses coalesce via singleflight, and storing sweeps expired entries. New flag
  `-scrape.perf-interval-cache-ttl` (default `10m`, `0` disables), SIGHUP
  reloadable. On vcsim the negotiation step goes from ~5.0 ms / 606 KiB / 5723
  allocs to ~3.3 ms / 391 KiB / 4474 allocs per scrape (-33% / -35% / -22%);
  the baseline already has the earlier CounterCache on, so the difference is
  isolated to this change. Metric output is unchanged; see
  `docs/perf/p-provider-summary-cache.txt`.

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

- **The scripted one-shot deployment bundle under `packaging/systemd/` is now
  held together by CI.** `check_packaging()` verifies that every key in
  `config.yaml` — including commented example lines an operator will uncomment —
  is a flag the binary actually registers, that the mapping stays flat as
  `config.go` requires, that the unit's `-file=` path and binary match the
  destinations `install.sh` installs to, that the unit carries `ExecReload`
  only while the binary handles SIGHUP, and that every file
  `scripts/build-systemd-pkg.sh` tars up exists. Before this, the bundle was
  validated by nothing, so a renamed flag could ship a config that aborts with
  "config sets unknown flag" at install time while every build stayed green.
  Reverse-verified by injecting an unknown key, a unit/install path mismatch,
  and a missing build input — each reported with its specific message.

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

- **The CI Lint job is green again.** golangci-lint v2.x reported three
  staticcheck findings, all in `_test.go`, that had left the job red across
  releases: two `QF1001` De Morgan's-law suggestions in
  `internal/config/config_test.go` (the positive condition is now named in a
  `consistent` bool, behaviour unchanged) and one `SA5000` "assignment to nil
  map" in `internal/collector/set_test.go`. The nil-map write is the *point* of
  `TestPanickingCollectorDoesNotKillTheProcess` — it must raise a runtime panic
  to exercise the collector recover path — so it carries a scoped
  `//nolint:staticcheck // SA5000 intentional` rather than being rewritten. The
  test still passes with `-count=1`, confirming the panic and the recover both
  still happen.

- **Dependabot is removed and this fork no longer tracks upstream.**
  `.github/dependabot.yml` is deleted, so no gomod / github-actions / docker
  update PRs are opened; action and dependency versions are bumped by hand from
  now on. The matching `check_dependabot()` guard in `scripts/check_config.py`
  (which previously *required* the file and every present ecosystem to be
  declared) is removed along with its docstring section, so the offline check
  stays green without the automation — `check_config.py` still reports OK
  (32 flags, 30 files scanned). The stale `codex/sync-upstream` branch, which
  carried no commits of its own and lagged `master` by 65, is deleted from the
  remote. There was never an `upstream` remote or a scheduled sync workflow, so
  upstream code only ever moved via an explicit fetch/merge or the GitHub "Sync
  fork" button; neither exists in normal use now. The fork's parent metadata on
  GitHub is display-only and pulls nothing by itself.

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

#### Powered-off VMs and maintenance hosts now appear in inventory metrics (v0.2.0)

Before v0.2.0, `vmware_vm_info` and `vmware_host_info` — together with their
capacity series — were emitted **only** for powered-on VMs and
connected/non-maintenance hosts. The same entities now always appear, with the
new state gauges carrying the reason. If a query, recording rule or dashboard
implicitly assumed "every series in `vmware_vm_info` is a running VM", its
counts and denominators change the moment a VM is shut down.

Find affected queries with a quick grep for the inventory metric names:

```sh
grep -rE 'vmware_(vm|host)_(info|cpu_corecount|mem_capacity)' \
  /etc/prometheus /var/lib/grafana/dashboards 2>/dev/null
```

The migration is a single `group_left` join onto the new state gauge, which you
can stage as a recording rule and migrate panels one at a time.

Powered-on VMs only (old `vmware_vm_info` semantics):

```promql
vmware_vm_info
  * on(vcenter, vmmo) group_left(state)
  vmware_vm_power_state{state="poweredOn"}
```

Connected, powered-on, non-maintenance hosts only (old `vmware_host_info`
semantics):

```promql
vmware_host_info
  and on(vcenter, hostmo) vmware_host_power_state{state="poweredOn"}
  and on(vcenter, hostmo) vmware_host_connection_state{state="connected"}
  and on(vcenter, hostmo) vmware_host_maintenance_mode == 0
```

(The state gauges carry their value in a `state` label rather than as distinct
metric names, so the filter is an `and` on the label, not a multiplication;
join with `* ... group_left(state)` instead when you want the state label on
the result, as the VM example does.)

Capacity totals need the same treatment. Old:

```promql
sum(vmware_vm_mem_capacity_bytes)                      # configured VM RAM
sum(vmware_host_mem_capacity_bytes)                    # host physical RAM
```

New — keep the denominator stable by counting only what used to be visible:

```promql
sum(
  vmware_vm_mem_capacity_bytes
    * on(vcenter, vmmo) group_left(state)
    vmware_vm_power_state{state="poweredOn"}
)

sum(
  vmware_host_mem_capacity_bytes
    and on(vcenter, hostmo) vmware_host_power_state{state="poweredOn"}
    and on(vcenter, hostmo) vmware_host_connection_state{state="connected"}
    and on(vcenter, hostmo) vmware_host_maintenance_mode == 0
)
```

Alternatively — and this is the better end state — keep the new all-entity
denominator and alert on the gap instead of hiding it:

```promql
# configured VM RAM sitting on powered-off VMs
sum(vmware_vm_mem_capacity_bytes)
  - sum(
      vmware_vm_mem_capacity_bytes
        * on(vcenter, vmmo) group_left(state)
        vmware_vm_power_state{state="poweredOn"}
    )
```

`vmware_scrape_entities_skipped` (reasons `powered_off`, `suspended`,
`disconnected`, `not_responding`, `maintenance`) gives the same visibility at
the collector level; alert on the raw gauge, since it is a per-scrape snapshot
rather than a counter. No series were renamed or removed in this change, and no
flag gates the new behaviour — the state gauges are the migration path.

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
