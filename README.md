
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
| -prom.maxRequests | Max concurrent scrape requests (default: 20) |
| -disable.exporter.metrics | Disables exporter process metrics |
| -disable.exporter.target | Disables exporter default target - /metrics will only return exporter data - use /probe |
| -disable.default.collectors | Disables all collectors enabled by default |
| -collector.datacenter | Enables or disables DataCenter metrics collection (default: enabled) |
| -collector.cluster | Enables or disables Cluster metrics collection (default: enabled) |
| -collector.datastore | Enables or disables Datastore metrics collection (default: enabled) |
| -collector.host | Enables or disables Host metrics collection (default: enabled) |
| -collector.vm | Enables or disables Virtual Machine metrics collection (default: enabled) |
| -collector.esxcli.host.nic | Collects ESXi NIC firmware information using esxcli over the SOAP API (proxied by vCenter, or direct when connected to an ESXi host) (default: disabled) |
| -collector.esxcli.storage | Collects ESXi storage firmware information using esxcli over the SOAP API (proxied by vCenter, or direct when connected to an ESXi host) (default: disabled) |
| -vmware.granularity | Time granularity of the sampled data in seconds. Must be > 0 and no greater than -vmware.interval (default 20) |
| -vmware.insecureTLS | Trust insecure TLS certificates (true) or verify them (default). ESXi hosts ship self-signed certificates, so this is usually needed for direct collection |
| -vmware.interval | PerfManager sampling window in seconds. This is a *request* - the effective interval is decided by the server's PerfProviderSummary.RefreshRate. No longer used for timeout calculation (default 20) |
| -vmware.timeout | Overall timeout in seconds for a single scrape, covering login, property retrieval and performance sampling (default 60) |
| -vmware.password | Password for the user above |
| -vmware.schema | Use HTTP or HTTPS (default "https") |
| -vmware.username | Username to login with |
| -vmware.vcenter | Target address in host:port format. Accepts a vCenter **or** a standalone ESXi host. This is not the vCenter Management Console. The flag name is kept for backwards compatibility |

Invalid values (for example `-vmware.granularity=0`, or a granularity larger than
the interval) make the process exit at startup with an explicit reason instead of
running with a broken configuration.


## Self-monitoring

Every scrape emits per-collector health metrics, so a collector that silently
fails is visible without reading logs:

```
vmware_scrape_collector_duration_seconds{collector="host"} 1.284
vmware_scrape_collector_success{collector="host"} 1
```

Alert on `min_over_time(vmware_scrape_collector_success[5m]) == 0` to catch a
collector that is consistently failing while the rest of the scrape succeeds.

Unknown collector names passed via `collect[]` are rejected with HTTP 400 rather
than silently ignored.

The esxcli collectors are a very specific use case that probably is not going to be needed by anyone. Left the code in here as an example on how custom information can be collected using esxcli command tool remotely via the SOAP API (`vim.EsxCLI.*`) — no SSH involved. 