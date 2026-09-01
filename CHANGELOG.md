# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Deprecated

Three metrics are superseded by explicitly unit-suffixed replacements. **Both
the old and the new names are emitted** for one release cycle, with identical
values, so you can migrate at your own pace. The deprecated ones carry a
`DEPRECATED:` marker in their help text and will be removed in a future release.

| Deprecated | Replacement | Why |
| --- | --- | --- |
| `vmware_host_cpu_capacity` | `vmware_host_cpu_capacity_mhz` | Name carried no unit |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | Help claimed MB, the value was always bytes |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | Help was copy-pasted from `mem_capacity` |

**No value changed.** In every case the numbers were already correct and only
the documentation was wrong, so the replacements do no unit conversion. If you
were compensating for the documented-but-wrong unit somewhere, remove the
correction.

`vmware_vm_mem_capacity` is deliberately **not** deprecated and has no `_bytes`
variant: `Summary.Config.MemorySizeMB` really is megabytes, so its help was
correct all along. Converting it would change the value, which is a different
class of breaking change.

### Added

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
  flag has no row in either README's reference table.
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

### Changed

- `/metrics` and `/probe` now share one scrape scheduler instead of maintaining
  two divergent code paths.
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
- `dependabot.yml` watches `github-actions` in addition to `gomod`. Only Go
  modules were configured, which is why the action versions above had been left
  behind: nothing was tracking them. Grouped into a single PR so the weekly bump
  stays reviewable.
- Removed dead code: `inSlice`, `moSliceToString`, five entirely
  commented-out files under `vmware/api/` (`clusters.go`, `datastores.go`,
  `host.go`, `vm.go`, `inventory.go` — remnants of a REST `/api/vcenter/...`
  implementation superseded by the SOAP path), and a commented-out manual SOAP
  block in `esxclistoragelist.go` that `esxcli.GetSOAP` replaced.
