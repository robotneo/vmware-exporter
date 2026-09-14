#!/usr/bin/env python3
"""Apply and enforce the v0.2.0 lifecycle semantics on the bundled dashboards.

The five dashboards (vmware-{vcenter,cluster,host,datastore}-overview and
vmware-vm-detail) are hand-authored against VictoriaMetrics. v0.2.0 made
``vmware_vm_info``/``vmware_host_info`` and the static capacity series appear for
every entity (powered-off VMs, disconnected/maintenance hosts), whereas before
they existed only for powered-on/eligible ones. Only perf counters still skip
ineligible entities, and a ``perf * on(...) group_left info`` inner join already
drops those by itself.

An estate aggregation written the old way therefore silently changes meaning:
``count(vmware_vm_info)`` labelled "running VMs" counts powered-off VMs, and a
host utilisation/overcommit ratio keeps a maintenance host's capacity in the
denominator while its usage numerator is empty. Nothing errors in Grafana; the
panel is just wrong.

Policy is explicit and keyed by (file, panel id):
  wrap(host|vm|both) intersect static refs with the eligible/powered-on set;
  override = full authored PromQL for shapes the wrap cannot handle;
  running  = a panel literally counting running VMs -> power_state poweredOn;
  keep     = intentionally raw ("inventory" enumerates all entities, the
             injected lifecycle row shows the split; "perf" is an inner join
             that already drops ineligible entities);
  drill    = single-entity drill-down, configured capacity shown on purpose.
Estate files must list every sensitive panel in POLICY or the check fails.
Running with no flag is idempotent and injects lifecycle panels; --check asserts
the on-disk files are the transform fixed point and nothing is unclassified.
"""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
DASHBOARD_DIR = REPO_ROOT / "dashboards"
BACKUP_SUFFIX = ".lifecycle.bak"

F_VC = "vmware-vcenter-overview.json"
F_CL = "vmware-cluster-overview.json"
F_HO = "vmware-host-overview.json"
F_DS = "vmware-datastore-overview.json"
F_VM = "vmware-vm-detail.json"
ALL_FILES = [F_VC, F_CL, F_HO, F_DS, F_VM]
ESTATE_FILES = {F_VC, F_CL}

HOST_ELIGIBLE = (
    '(vmware_host_power_state{state="poweredOn"} '
    'and on(vcenter, hostmo) vmware_host_connection_state{state="connected"} '
    'and on(vcenter, hostmo) vmware_host_maintenance_mode == 0)'
)
VM_POWERED_ON = 'vmware_vm_power_state{state="poweredOn"}'

_HS = r"mem_capacity_bytes|cpu_corecount|cpu_threadcount|cpu_capacity_hertz"
HOST_STATIC_RE = re.compile(r"vmware_host_(?:" + _HS + r")\s*\{[^}]*\}")
HOST_INFO_RE = re.compile(r"vmware_host_info\s*\{[^}]*\}")
VM_ACCT_RE = re.compile(r"vmware_vm_(?:mem_capacity_bytes|cpu_corecount)\s*\{[^}]*\}")
SENSITIVE_RE = re.compile(
    r"vmware_(?:host_(?:info|" + _HS + r")|vm_(?:info|mem_capacity_bytes|cpu_corecount))"
)
STATE_RE = re.compile(r"vmware_(?:vm|host)_(?:power_state|connection_state|maintenance_mode)")

POLICY: dict[str, dict[int, tuple[str, str]]] = {
    F_VC: {
        12: ("keep", "inventory"), 13: ("running", ""),
        16: ("wrap", "host"), 17: ("wrap", "host"),
        21: ("wrap", "host"), 22: ("wrap", "host"), 23: ("wrap", "host"),
        24: ("wrap", "host"), 27: ("wrap", "host"), 28: ("wrap", "host"),
        29: ("wrap", "both"), 30: ("wrap", "both"), 31: ("wrap", "vm"),
        32: ("wrap", "both"),
        34: ("wrap", "host"), 35: ("wrap", "host"), 36: ("wrap", "host"),
        37: ("wrap", "host"), 43: ("wrap", "host"), 44: ("wrap", "host"),
        50: ("keep", "inventory"), 51: ("keep", "inventory"),
    },
    F_CL: {
        102: ("keep", "inventory"), 103: ("wrap", "host"), 104: ("wrap", "host"),
        105: ("keep", "perf"),
        106: ("wrap", "vm"), 107: ("wrap", "vm"),
        108: ("wrap", "host"), 109: ("wrap", "host"),
        110: ("wrap", "both"), 111: ("wrap", "both"), 112: ("wrap", "both"),
        113: ("wrap", "both"), 114: ("wrap", "host"),
        121: ("keep", "inventory"), 122: ("keep", "perf"), 123: ("keep", "perf"),
        125: ("wrap", "host"), 126: ("wrap", "host"), 127: ("wrap", "host"),
        128: ("override", ""), 129: ("wrap", "host"),
        130: ("keep", "perf"), 134: ("keep", "perf"), 135: ("keep", "perf"),
        116: ("keep", "perf"),
        139: ("override", ""), 140: ("wrap", "host"),
        141: ("keep", "perf"), 142: ("keep", "inventory"),
        143: ("wrap", "host"), 144: ("wrap", "host"),
    },
    F_HO: {
        105: ("running", ""), 107: ("wrap", "vm"), 108: ("wrap", "vm"),
        155: ("keep", "inventory"),
    },
}

# Per-cluster CPU ratio, authored explicitly because the denominator's first
# factor has no inline selector (it relies on the * on(hostmo) cluster join).
# Parentheses matter: `and` binds looser than `*`, so the eligible intersection
# is grouped FIRST (keeping hostmo), then cluster-joined to add vmwcluster.
_CLUSTER_CPU_RATIO = (
    '100 * sum by(vmwcluster)('
    'vmware_host_cpu_usage_hertz{vcenter=~"$vcenter",hostmo=~"$hostmo"} '
    '* on(hostmo) group_left(vmwcluster) '
    '(vmware_host_info{vcenter=~"$vcenter",hostmo=~"$hostmo"} '
    '* on(cmo) group_left(vmwcluster) '
    'vmware_cluster_info{vcenter=~"$vcenter",vmwcluster=~"$cluster"})) '
    '/ sum by(vmwcluster)('
    '((vmware_host_cpu_capacity_hertz * vmware_host_cpu_corecount'
    '{vcenter=~"$vcenter",hostmo=~"$hostmo"}) '
    'and on(vcenter, hostmo) ' + HOST_ELIGIBLE + ') '
    '* on(hostmo) group_left(vmwcluster) '
    '(vmware_host_info{vcenter=~"$vcenter",hostmo=~"$hostmo"} '
    '* on(cmo) group_left(vmwcluster) '
    'vmware_cluster_info{vcenter=~"$vcenter",vmwcluster=~"$cluster"}))'
)
OVERRIDES = {(F_CL, 128, 0): _CLUSTER_CPU_RATIO, (F_CL, 139, 0): _CLUSTER_CPU_RATIO}
RUNNING_OVERRIDES = {
    (F_VC, 13, 0): 'count(vmware_vm_info{vcenter=~"$vcenter"} and on(vcenter, vmmo) ' + VM_POWERED_ON + ")",
    (F_HO, 105, 0): 'count(vmware_vm_info{vcenter=~"$vcenter", hostmo=~"$hostmo"} and on(vcenter, vmmo) ' + VM_POWERED_ON + ")",
}


def _wrap_host(m):
    return f"({m.group(0)} and on(vcenter, hostmo) {HOST_ELIGIBLE})"


def _wrap_vm(m):
    return f"({m.group(0)} and on(vcenter, vmmo) {VM_POWERED_ON})"


def wrap_expression(expr, scope):
    if STATE_RE.search(expr):
        return expr
    if scope in ("host", "both"):
        expr = HOST_STATIC_RE.sub(_wrap_host, expr)
        expr = HOST_INFO_RE.sub(_wrap_host, expr)
    if scope in ("vm", "both"):
        expr = VM_ACCT_RE.sub(_wrap_vm, expr)
    return expr


def iter_panels(node):
    for panel in node.get("panels") or []:
        yield panel
        yield from iter_panels(panel)


def bottom_y(data):
    return max(int(p.get("gridPos", {}).get("y", 0)) + int(p.get("gridPos", {}).get("h", 0))
               for p in iter_panels(data))


# --- Grafana (schemaVersion 39) builder, datasource = VictoriaMetrics -------
DS_VM = {"type": "victoriametrics-metrics-datasource",
         "uid": "${DS_VICTORIAMETRICS-METRICS-DATASOURCE}"}


def _target(ref, expr, legend="", instant=False):
    t = {"datasource": DS_VM, "expr": expr, "refId": ref,
         "format": "table" if instant else "time_series",
         "instant": instant, "range": not instant}
    if legend:
        t["legendFormat"] = legend
    return t


def _thresholds(steps):
    return {"mode": "absolute", "steps": steps}


def _mk_stat(pid, title, grid, expr, unit, display_name=None, mappings=None, steps=None):
    defaults = {"mappings": mappings or [], "unit": unit or "short",
                "thresholds": _thresholds(steps or [{"color": "green", "value": None}])}
    if display_name:
        defaults["displayName"] = display_name
    return {
        "id": pid, "type": "stat", "title": title, "datasource": DS_VM,
        "gridPos": grid, "targets": [_target("A", expr)],
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "options": {"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
                    "orientation": "horizontal", "reduceOptions": {
                        "calcs": ["lastNotNull"], "fields": "", "values": False},
                    "textMode": "auto", "showPercentChange": False},
    }


def _mk_ts(pid, title, grid, series, unit):
    return {
        "id": pid, "type": "timeseries", "title": title, "datasource": DS_VM,
        "gridPos": grid,
        "targets": [_target(chr(65 + i), e, l) for i, (e, l) in enumerate(series)],
        "fieldConfig": {"defaults": {"custom": {
            "drawStyle": "line", "lineInterpolation": "linear", "fillOpacity": 12,
            "stacking": {"mode": "none", "group": "A"}, "showPoints": "never",
            "spanNulls": False}, "unit": unit or "short"}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom",
                               "calcs": ["lastNotNull"]},
                    "tooltip": {"mode": "multi", "sort": "desc"}},
    }


def _mk_table(pid, title, grid, expr):
    return {
        "id": pid, "type": "table", "title": title, "datasource": DS_VM,
        "gridPos": grid, "targets": [_target("A", expr, instant=True)],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "filterable": True},
                                     "unit": "short"}, "overrides": []},
        "options": {"showHeader": True, "cellHeight": "sm",
                    "footer": {"show": False, "reducer": ["sum"]}},
    }


def _mk_row(pid, title, y):
    return {"id": pid, "type": "row", "title": title, "collapsed": False,
            "datasource": DS_VM, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
            "panels": []}


def build_blocks(spec, start_y):
    """Expand the compact spec into positioned Grafana panels, ids from 900."""
    panels = []
    pid = 900
    x, y, rowh = 0, start_y, 0
    for item in spec:
        kind = item[0]
        if kind == "row":
            y = y + rowh
            x, rowh = 0, 0
            panels.append(_mk_row(pid, item[1], y)); pid += 1
            y += 1
            continue
        title = item[1]
        if kind == "stat":
            w, h = 6, 4
            extra = item[4] if len(item) > 4 and item[4] else {}
            grid_panel = lambda g: _mk_stat(
                pid, title, g, item[2], item[3],
                display_name=extra.get("display"),
                mappings=extra.get("mappings"), steps=extra.get("steps"))
        elif kind == "ts":
            w, h = 12, 8
            grid_panel = lambda g: _mk_ts(pid, title, g, item[2], item[3])
        else:  # table
            w, h = 24, 11
            grid_panel = lambda g: _mk_table(pid, title, g, item[2])
        if x + w > 24:
            y += rowh; x, rowh = 0, 0
        panels.append(grid_panel({"h": h, "w": w, "x": x, "y": y})); pid += 1
        x += w; rowh = max(rowh, h)
    return panels


VC = 'vcenter=~"$vcenter"'
CL_C = 'vcenter=~"$vcenter",cmo=~"$clustermo"'
CL_H = 'vcenter=~"$vcenter",hostmo=~"$hostmo"'
HO_S = 'vcenter=~"$vcenter",hostmo=~"$hostmo"'
VM_S = 'vcenter=~"$vcenter",vmmo=~"$vmmo"'
DS_S = 'vcenter=~"$vcenter",dsmo=~"$dsmo"'

# Powered-off/suspended VMs scoped to the cluster/host need a vm_info join:
# vm power_state has no hostmo label, vm_info does.
def _vm_state_on_hosts(state):
    return (
        f'count(vmware_vm_power_state{{{VC},state="{state}"}} '
        '* on(vcenter, vmmo) group_left(hostmo) '
        f'max by(vcenter, vmmo, hostmo)(vmware_vm_info{{{CL_H}}}))'
    )


LIFECYCLE = {
    F_VC: [
        ("row", "生命周期与健康 · Lifecycle & Health"),
        ("stat", "关机 VM · Powered Off", f'count by(vcenter)(vmware_vm_power_state{{{VC},state="poweredOff"}})', None),
        ("stat", "挂起 VM · Suspended", f'count by(vcenter)(vmware_vm_power_state{{{VC},state="suspended"}})', None),
        ("stat", "维护中主机 · Maintenance", f'count by(vcenter)(vmware_host_maintenance_mode{{{VC}}} == 1)', None),
        ("stat", "断连主机 · Disconnected", f'count by(vcenter)(vmware_host_connection_state{{{VC},state="disconnected"}})', None),
        ("stat", "无响应主机 · Not Responding", f'count by(vcenter)(vmware_host_connection_state{{{VC},state="notResponding"}})', None),
        ("stat", "非绿健康实体 · Not Green",
         f'count(vmware_host_overall_status{{{VC},status!="green"}}) + '
         f'count(vmware_vm_overall_status{{{VC},status!="green"}})', None),
        ("ts", "主机电源 / 连接 / 维护分布", [
            (f'count by(state)(vmware_host_power_state{{{VC}}})', "power {{state}}"),
            (f'count by(state)(vmware_host_connection_state{{{VC}}})', "connection {{state}}"),
            (f'count(vmware_host_maintenance_mode{{{VC}}} == 1)', "maintenance"),
        ], None),
        ("ts", "VM 电源状态分布", [
            (f'count by(state)(vmware_vm_power_state{{{VC}}})', "{{state}}"),
        ], None),
        ("ts", "非绿 overall_status 实体数", [
            (f'count by(status)(vmware_host_overall_status{{{VC},status!="green"}})', "host {{status}}"),
            (f'count by(status)(vmware_vm_overall_status{{{VC},status!="green"}})', "vm {{status}}"),
            (f'count by(status)(vmware_datastore_overall_status{{{VC},status!="green"}})', "datastore {{status}}"),
            (f'count by(status)(vmware_cluster_overall_status{{{VC},status!="green"}})', "cluster {{status}}"),
        ], None),
        ("ts", "采集实体：发现 / 输出 / 跳过", [
            (f'sum by(collector, kind)(vmware_scrape_entities_found{{{VC}}})', "found {{collector}}/{{kind}}"),
            (f'sum by(collector, kind)(vmware_scrape_entities_emitted{{{VC}}})', "emitted {{collector}}/{{kind}}"),
            (f'sum by(reason)(vmware_scrape_entities_skipped{{{VC}}})', "skipped {{reason}}"),
        ], None),
    ],
    F_CL: [
        ("row", "生命周期与有效容量 · Lifecycle & Effective Capacity"),
        ("stat", "有效主机 · Effective Hosts", f'min(vmware_cluster_effective_hosts{{{CL_C}}})', None),
        ("stat", "关机 VM · Powered Off", _vm_state_on_hosts("poweredOff"), None),
        ("stat", "挂起 VM · Suspended", _vm_state_on_hosts("suspended"), None),
        ("stat", "维护中主机 · Maintenance", f'count(vmware_host_maintenance_mode{{{CL_H}}} == 1)', None),
        ("stat", "断连 / 无响应主机", f'count(vmware_host_connection_state{{{CL_H},state=~"disconnected|notResponding"}})', None),
        ("stat", "非绿集群 · Not Green", f'count(vmware_cluster_overall_status{{{CL_C},status!="green"}})', None),
        ("ts", "集群 CPU：总容量 vs 有效容量", [
            (f'sum(vmware_cluster_cpu_capacity_hertz{{{CL_C}}})', "total capacity"),
            (f'sum(vmware_cluster_cpu_effective_hertz{{{CL_C}}})', "effective"),
        ], "hertz"),
        ("ts", "集群内存：总容量 vs 有效容量", [
            (f'sum(vmware_cluster_memory_capacity_bytes{{{CL_C}}})', "total capacity"),
            (f'sum(vmware_cluster_memory_effective_bytes{{{CL_C}}})', "effective"),
        ], "bytes"),
        ("ts", "集群整体状态与各主机状态", [
            (f'max by(status)(vmware_cluster_overall_status{{{CL_C}}})', "cluster {{status}}"),
            (f'count by(state)(vmware_host_connection_state{{{CL_H}}})', "host connection {{state}}"),
            (f'count(vmware_host_maintenance_mode{{{CL_H}}} == 1)', "host maintenance"),
        ], None),
        ("ts", "采集实体：发现 / 输出 / 跳过", [
            (f'sum by(collector, kind)(vmware_scrape_entities_found{{{VC}}})', "found {{collector}}/{{kind}}"),
            (f'sum by(collector, kind)(vmware_scrape_entities_emitted{{{VC}}})', "emitted {{collector}}/{{kind}}"),
            (f'sum by(reason)(vmware_scrape_entities_skipped{{{VC}}})', "skipped {{reason}}"),
        ], None),
    ],
    F_HO: [
        ("row", "生命周期与健康 · Lifecycle & Health"),
        ("stat", "主机电源 · Power", f'max by(state)(vmware_host_power_state{{{HO_S}}})', None,
         {"display": "${__field.labels.state}"}),
        ("stat", "连接状态 · Connection", f'max by(state)(vmware_host_connection_state{{{HO_S}}})', None,
         {"display": "${__field.labels.state}"}),
        ("stat", "维护模式 · Maintenance", f'max(vmware_host_maintenance_mode{{{HO_S}}})', None,
         {"mappings": [{"type": "value", "options": {
             "0": {"text": "否", "color": "green"},
             "1": {"text": "维护中", "color": "orange"}}}]}),
        ("stat", "整体健康 · Overall Status", f'max by(status)(vmware_host_overall_status{{{HO_S}}})', None,
         {"display": "${__field.labels.status}"}),
        ("stat", "本机关机 VM · Powered Off",
         f'count(vmware_vm_power_state{{{VC},state="poweredOff"}} * on(vcenter, vmmo) '
         f'group_left(hostmo) max by(vcenter, vmmo, hostmo)(vmware_vm_info{{{HO_S}}}))', None),
        ("stat", "本机挂起 VM · Suspended",
         f'count(vmware_vm_power_state{{{VC},state="suspended"}} * on(vcenter, vmmo) '
         f'group_left(hostmo) max by(vcenter, vmmo, hostmo)(vmware_vm_info{{{HO_S}}}))', None),
        ("ts", "本机 VM 电源状态分布", [
            (f'count by(state)(vmware_vm_power_state{{{VC}}} * on(vcenter, vmmo) '
             f'group_left(hostmo) max by(vcenter, vmmo, hostmo)(vmware_vm_info{{{HO_S}}}))', "{{state}}"),
        ], None),
        ("ts", "采集实体：发现 / 输出 / 跳过", [
            (f'sum by(collector, kind)(vmware_scrape_entities_found{{{VC}}})', "found {{collector}}/{{kind}}"),
            (f'sum by(collector, kind)(vmware_scrape_entities_emitted{{{VC}}})', "emitted {{collector}}/{{kind}}"),
            (f'sum by(reason)(vmware_scrape_entities_skipped{{{VC}}})', "skipped {{reason}}"),
        ], None),
    ],
    F_DS: [
        ("row", "健康状态 · Health"),
        ("stat", "不可访问数据存储 · Inaccessible", f'count(vmware_datastore_accessible{{{DS_S}}} == 0)', None),
        ("stat", "非绿数据存储 · Not Green", f'count(vmware_datastore_overall_status{{{DS_S},status!="green"}})', None),
        ("ts", "数据存储 overall_status 分布", [
            (f'count by(status)(vmware_datastore_overall_status{{{VC}}})', "{{status}}"),
        ], None),
        ("ts", "可访问性（按数据存储）", [
            (f'max by(ds)(vmware_datastore_accessible{{{VC}}})', "{{ds}}"),
        ], None),
    ],
    F_VM: [
        ("row", "生命周期 · Lifecycle"),
        ("stat", "电源状态 · Power State", f'max by(state)(vmware_vm_power_state{{{VM_S}}})', None,
         {"display": "${__field.labels.state}",
          "mappings": [{"type": "value", "options": {
              "poweredOn": {"text": "已开机 poweredOn", "color": "green"},
              "poweredOff": {"text": "已关机 poweredOff", "color": "red"},
              "suspended": {"text": "挂起 suspended", "color": "yellow"}}}]}),
        ("stat", "整体健康 · Overall Status", f'max by(status)(vmware_vm_overall_status{{{VM_S}}})', None,
         {"display": "${__field.labels.status}"}),
        ("ts", "VM 抓取跳过原因（本 vCenter）", [
            (f'sum by(reason)(vmware_scrape_entities_skipped{{{VC},collector="vm"}})', "{{reason}}"),
        ], None),
    ],
}

def transform_expressions(data, filename):
    """Apply the per-panel policy in place. Returns (changed, notes)."""
    changed = 0
    notes = []
    table = POLICY.get(filename, {})
    estate = filename in ESTATE_FILES
    for panel in iter_panels(data):
        pid = panel.get("id")
        if pid is None or pid >= 900:
            continue  # injected lifecycle panels are already correct
        action, scope = table.get(pid, (None, ""))
        for ti, target in enumerate(panel.get("targets") or []):
            expr = target.get("expr")
            if not isinstance(expr, str) or not expr.strip():
                continue
            new = None
            why = None
            key = (filename, pid, ti)
            if key in OVERRIDES:
                new, why = OVERRIDES[key], "override"
            elif key in RUNNING_OVERRIDES:
                new, why = RUNNING_OVERRIDES[key], "running"
            elif action == "wrap":
                new, why = wrap_expression(expr, scope), f"wrap({scope})"
            elif action == "running":
                # Generic safety net for any running panel without an authored form.
                if SENSITIVE_RE.search(expr) and "count(" in expr:
                    new, why = expr, "running-unhandled"
            elif action in ("keep", "drill"):
                why = f"keep({scope})"
            elif estate and SENSITIVE_RE.search(expr):
                # Estate file: a sensitive panel with no policy is an error,
                # surfaced by check_dashboard; leave untouched here.
                continue
            else:
                # Drill file default, or an insensitive expression.
                continue
            if new is not None and new != expr:
                target["expr"] = new
                changed += 1
                notes.append(f"  [{pid}] {panel.get('title')}: {why}")
    return changed, notes


def check_dashboard(data, filename):
    """Audit one dashboard. Returns a list of human-readable problems."""
    problems = []
    table = POLICY.get(filename, {})
    estate = filename in ESTATE_FILES

    for panel in iter_panels(data):
        pid = panel.get("id")
        if pid is None or pid >= 900:
            continue
        for ti, target in enumerate(panel.get("targets") or []):
            expr = target.get("expr") if isinstance(target, dict) else None
            if not isinstance(expr, str) or not SENSITIVE_RE.search(expr):
                continue
            key = (filename, pid, ti)
            if key in OVERRIDES:
                want = OVERRIDES[key]
                if " ".join(expr.split()) != " ".join(want.split()):
                    problems.append(f"[{pid} {panel.get('title')}] override drifted")
                continue
            if key in RUNNING_OVERRIDES:
                want = RUNNING_OVERRIDES[key]
                if " ".join(expr.split()) != " ".join(want.split()):
                    problems.append(f"[{pid} {panel.get('title')}] running-VM count is not powered-on filtered")
                continue
            action, scope = table.get(pid, (None, ""))
            if action is None:
                if estate:
                    problems.append(
                        f"[{pid} {panel.get('title')}] lifecycle-sensitive panel with no policy "
                        f"classification: {' '.join(expr.split())[:120]}"
                    )
                continue  # drill file default: intentionally raw
            if action == "wrap":
                re_wrapped = wrap_expression(expr, scope)
                if " ".join(re_wrapped.split()) != " ".join(expr.split()):
                    problems.append(
                        f"[{pid} {panel.get('title')}] static inventory not intersected with the "
                        f"eligible/powered-on set ({scope})"
                    )
            # keep / running(generic) are intentionally unfiltered -> OK.

    if not any(p.get("id", 0) >= 900 for p in iter_panels(data)):
        problems.append("lifecycle health panels (id >= 900) are missing")

    # Idempotency: a fixed point must transform to itself.
    probe = json.loads(json.dumps(data))
    n, _ = transform_expressions(probe, filename)
    if n:
        problems.append(f"transform is not idempotent ({n} expression(s) still change)")
    return problems


def detect_indent(path):
    widths = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.lstrip(" ")
        if stripped != line and stripped.strip():
            widths.add(len(line) - len(stripped))
    return min(widths) if widths else 2


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--check", action="store_true", help="report only, exit 1 if stale")
    args = ap.parse_args()

    failed = False
    for name in ALL_FILES:
        path = DASHBOARD_DIR / name
        if not path.exists():
            failed = True
            print(f"{name}: MISSING")
            continue
        data = json.loads(path.read_text(encoding="utf-8"))

        if args.check:
            problems = check_dashboard(data, name)
            if problems:
                failed = True
                print(f"{name}: STALE")
                for p in problems:
                    print(f"  - {p}")
            else:
                print(f"{name}: OK")
            continue

        n, notes = transform_expressions(data, name)
        if not any(p.get("id", 0) >= 900 for p in iter_panels(data)):
            blocks = build_blocks(LIFECYCLE[name], bottom_y(data) + 1)
            data.setdefault("panels", []).extend(blocks)
            injected = len(blocks)
        else:
            injected = 0
        if n == 0 and injected == 0:
            print(f"{name}: already up to date")
            continue

        backup = path.with_suffix(path.suffix + BACKUP_SUFFIX)
        if not backup.exists():
            backup.write_bytes(path.read_bytes())
        indent = detect_indent(path)
        path.write_text(json.dumps(data, indent=indent, ensure_ascii=False) + "\n",
                        encoding="utf-8")

        written = json.loads(path.read_text(encoding="utf-8"))
        problems = check_dashboard(written, name)
        if problems:
            failed = True
            print(f"{name}: UPDATED BUT AUDIT FAILED")
            for p in problems:
                print(f"  - {p}")
        else:
            print(f"{name}: updated ({n} expr, +{injected} lifecycle panels)")
        for line in notes:
            print(line)

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())

