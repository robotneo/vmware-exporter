#!/usr/bin/env python3
"""Migrate the bundled Grafana dashboards to the v0.2.0 lifecycle semantics.

Why this exists
---------------
v0.2.0 stopped dropping powered-off VMs and disconnected/maintenance hosts from
the *inventory* metrics (``vmware_vm_info``, ``vmware_host_info`` and the static
capacity series). Before v0.2.0 those series only existed for powered-on VMs and
connected, non-maintenance hosts, so an expression like::

    sum(vmware_host_mem_capacity_bytes)
    count(vmware_vm_info)

implicitly meant "eligible hosts" / "running VMs". After v0.2.0 the same
expression silently counts every host and every VM: a host placed in
maintenance no longer shrinks the numerator (perf counters are still skipped for
it) but stays in the denominator, so every utilisation percentage drops during
a maintenance window, and "Running VMs" starts reporting powered-off VMs.

The performance counters are unaffected -- vCenter only returns real-time
samples for powered-on VMs and eligible hosts, so a ``perf * group_left vm_info``
inner join already drops the ineligible entities by itself.

Migration policy (deliberately conservative)
---------------------------------------------
The bundled *overview* dashboards keep their v0.1.20 numbers. Every static
inventory/capacity reference in an estate- or cluster-scope aggregation is
intersected with an explicit eligibility set, instead of relying on the old
implicit filtering:

    host eligible = poweredOn AND connected AND maintenance_mode == 0
    vm   eligible = power_state{state="poweredOn"}

Single-entity drill-downs (vm-view, host-view, and the per-host block in
cluster-view keyed on ``host=~"$host"``) are left untouched on purpose: you
only reach them by selecting one specific host/VM, and seeing its configured
capacity while it is powered off or in maintenance is the point of the
lifecycle feature. The one exception is any panel literally titled
"Running VMs", which is filtered to powered-on VMs everywhere because that is
what the title promises.

The transform is structural (per metric reference), not a blind search/replace,
and ``check()`` fails the run if it finds a panel that references a sensitive
inventory metric but was neither migrated nor explicitly whitelisted -- a new
panel of that shape cannot be forgotten.

Usage
-----
    python3 scripts/lifecycle_dashboards.py            # migrate in place (.bak)
    python3 scripts/lifecycle_dashboards.py --check    # report only, exit 1 if stale
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

# Host static inventory/capacity series that v0.2.0 began emitting for
# powered-off / disconnected / maintenance hosts too.
HOST_STATIC_RE = re.compile(
    r"vmware_host_(?:mem_capacity_bytes|cpu_corecount|cpu_threadcount|cpu_capacity_hertz)"
    r"(?:\s*\{[^}]*\})?"
)
HOST_INFO_RE = re.compile(r"vmware_host_info(?:\s*\{[^}]*\})?")
# VM static accounting series (configured, present for powered-off VMs now).
VM_VALUE_RE = re.compile(
    r"vmware_vm_(?:mem_capacity_bytes|cpu_corecount)(?:\s*\{[^}]*\})?"
)
VM_INFO_RE = re.compile(r"vmware_vm_info(?:\s*\{[^}]*\})?")

# Already lifecycle-aware.
STATE_METRIC_RE = re.compile(
    r"vmware_(?:vm|host)_(?:power_state|connection_state|maintenance_mode)"
)

ELIGIBLE_HOSTS = (
    "(vmware_host_power_state{state=\"poweredOn\"} "
    "and on(vcenter, hostmo) vmware_host_connection_state{state=\"connected\"} "
    "and on(vcenter, hostmo) vmware_host_maintenance_mode == 0)"
)
POWERED_ON_VMS = "vmware_vm_power_state{state=\"poweredOn\"}"

SENSITIVE_RE = re.compile(
    r"vmware_(?:host_(?:info|mem_capacity_bytes|cpu_corecount|cpu_threadcount|cpu_capacity_hertz)"
    r"|vm_(?:info|mem_capacity_bytes|cpu_corecount))"
)

# Estate overview files: aggregate scope. Drill-down files are keyed below.
ESTATE_FILES = {"vmware-vcenter-view.json", "vmware-cluster-view.json"}
DRILL_FILES = {"vmware-vm-view.json", "vmware-host-view.json"}


def wrap_host(match: re.Match) -> str:
    return f"({match.group(0)} and on(vcenter, hostmo) {ELIGIBLE_HOSTS})"


def wrap_host_info(match: re.Match) -> str:
    return f"({match.group(0)} and on(vcenter, hostmo) {ELIGIBLE_HOSTS})"


def wrap_vm_value(match: re.Match) -> str:
    return f"({match.group(0)} and on(vcenter, vmmo) {POWERED_ON_VMS})"


def wrap_vm_info(match: re.Match) -> str:
    return f"({match.group(0)} and on(vcenter, vmmo) {POWERED_ON_VMS})"


# count( INNER ) -> count( (INNER) and on(vcenter, vmmo) powered-on )
RUNNING_COUNT_RE = re.compile(r"^\s*count\(\s*(?P<inner>.*)\s*\)\s*$", re.DOTALL)


def filter_running_vms(expr: str) -> tuple[str, bool]:
    match = RUNNING_COUNT_RE.match(expr)
    if not match:
        return expr, False
    inner = match.group("inner").strip()
    return (
        "count(\n  ("
        + inner
        + ")\n  and on(vcenter, vmmo) "
        + POWERED_ON_VMS
        + "\n)"
    ), True


def panel_own_title(panel: dict) -> str:
    return panel.get("title", "") or ""


def is_drill_expr(expr: str) -> bool:
    """A per-host drill block in cluster-view selects one host by name."""
    return 'host=~"$host"' in expr


def migrate_expr(expr: str, estate_panel: bool, running_panel: bool) -> tuple[str, str]:
    """Return (new_expr, decision). decision describes what happened / why kept."""
    if STATE_METRIC_RE.search(expr):
        return expr, "already lifecycle-aware"

    # The panel literally counts "Running VMs": constrain to poweredOn no
    # matter where it lives (estate row or per-host drill block).
    if running_panel:
        new, ok = filter_running_vms(expr)
        if ok:
            return new, "Running VMs: constrained to poweredOn"
        return expr, "KEPT: Running VMs panel without count() shape -- REVIEW"

    if not estate_panel:
        # Drill-down / perf inner-join: no eligibility wrapping.
        if "group_left" in expr:
            return expr, "perf/name inner join (ineligible entities drop automatically)"
        if SENSITIVE_RE.search(expr):
            return expr, "single-entity drill-down (configured value shown on purpose)"
        return expr, "unaffected"

    # Estate aggregate panel: intersect every static reference with eligibility.
    new = expr
    notes = []

    if HOST_STATIC_RE.search(new):
        new = HOST_STATIC_RE.sub(wrap_host, new)
        notes.append("host capacity intersected with eligible-host set")
    if HOST_INFO_RE.search(new) and "group_left" not in new:
        new = HOST_INFO_RE.sub(wrap_host_info, new)
        notes.append("host count intersected with eligible-host set")
    if VM_VALUE_RE.search(new):
        new = VM_VALUE_RE.sub(wrap_vm_value, new)
        notes.append("vm accounting intersected with powered-on set")
    if VM_INFO_RE.search(new) and "group_left" not in new:
        new = VM_INFO_RE.sub(wrap_vm_info, new)
        notes.append("vm count intersected with powered-on set")

    if not notes:
        # Estate panel referencing a sensitive token only through a group_left
        # name join (topk perf panels, etc.).
        if SENSITIVE_RE.search(expr) and "group_left" in expr:
            return expr, "perf/name inner join (ineligible entities drop automatically)"
        return expr, "unaffected"

    return new, "; ".join(notes)


def iter_targets(node: dict):
    for panel in node.get("panels") or []:
        yield panel
        yield from iter_targets(panel)


def transform_dashboard(data: dict, filename: str) -> tuple[bool, list[str]]:
    estate_file = filename in ESTATE_FILES
    changed = False
    report: list[str] = []

    for panel in iter_targets(data):
        title = panel_own_title(panel)
        running_panel = title == "Running VMs"
        for target in panel.get("targets") or []:
            if not isinstance(target, dict):
                continue
            expr = target.get("expr")
            if not isinstance(expr, str) or not expr:
                continue

            # cluster-view mixes estate rows with per-host drill rows; decide
            # per expression. vcenter-view is estate-only; drill files never
            # wrap unless this is the "Running VMs" panel.
            estate_panel = estate_file and not (
                filename == "vmware-cluster-view.json" and is_drill_expr(expr)
            )

            new, decision = migrate_expr(expr, estate_panel, running_panel)
            if new != expr:
                target["expr"] = new
                changed = True
                report.append(f"  MIGRATED [{title}] {decision}")
            elif decision not in ("unaffected",):
                report.append(f"  kept     [{title}] {decision}")

    return changed, report


def check_dashboard(data: dict, filename: str) -> list[str]:
    """Audit an already-migrated dashboard for unclassified sensitive panels."""
    problems: list[str] = []
    for panel in iter_targets(data):
        title = panel_own_title(panel)
        for target in panel.get("targets") or []:
            expr = target.get("expr") if isinstance(target, dict) else None
            if not isinstance(expr, str) or not SENSITIVE_RE.search(expr):
                continue
            if STATE_METRIC_RE.search(expr):
                continue  # migrated / lifecycle-aware
            if "group_left" in expr:
                continue  # perf/name inner join -- ineligible entities drop
            if title == "Running VMs":
                problems.append(f"[{title}] Running VMs count is not powered-on filtered")
                continue
            if filename in DRILL_FILES:
                continue  # single-entity drill-down, intentional
            if filename == "vmware-cluster-view.json" and is_drill_expr(expr):
                continue  # per-host drill block, intentional
            problems.append(
                f"[{title}] references a lifecycle-sensitive inventory metric "
                f"without an eligibility filter: {' '.join(expr.split())[:160]}"
            )
    return problems


def detect_indent(path: pathlib.Path) -> int:
    widths = set()
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            stripped = line.lstrip(" ")
            if stripped != line and stripped.strip():
                widths.add(len(line) - len(stripped))
    return min(widths) if widths else 2


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--check", action="store_true", help="report only, exit 1 if stale")
    args = parser.parse_args()

    files = sorted(p for p in DASHBOARD_DIR.glob("vmware-*.json"))
    failed = False

    for path in files:
        data = json.loads(path.read_text(encoding="utf-8"))

        if args.check:
            problems = check_dashboard(data, path.name)
            if problems:
                failed = True
                print(f"{path.name}: STALE")
                for problem in problems:
                    print(f"  - {problem}")
            else:
                print(f"{path.name}: OK")
            continue

        changed, report = transform_dashboard(data, path.name)
        if not changed:
            print(f"{path.name}: already migrated")
            continue

        backup = path.with_suffix(path.suffix + BACKUP_SUFFIX)
        if not backup.exists():
            backup.write_bytes(path.read_bytes())

        indent = detect_indent(path)
        with path.open("w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=indent, ensure_ascii=False)
            fh.write("\n")

        # Verify the written file passes the audit and still parses.
        written = json.loads(path.read_text(encoding="utf-8"))
        problems = check_dashboard(written, path.name)
        if problems:
            failed = True
            print(f"{path.name}: MIGRATED BUT AUDIT FAILED")
            for problem in problems:
                print(f"  - {problem}")
        else:
            print(f"{path.name}: migrated")
        for line in report:
            print(line)

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
