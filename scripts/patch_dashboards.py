#!/usr/bin/env python3
"""Inject the target_type template variable into the bundled Grafana dashboards.

Why a script instead of hand-editing
------------------------------------
The dashboard JSON files are 35-80 KB of machine-generated Grafana state. Editing
them by hand is how you get a dashboard that loads but silently renders nothing.
This script makes the change reproducible, reviewable and revertible: it loads the
JSON, mutates a well-defined location, writes it back with stable formatting, and
then reads it back to prove the result is still valid JSON.

What it changes
---------------
1. Adds a ``target_type`` template variable, sourced from
   ``label_values(vmware_target_info{job="$job"}, type)``.
2. Rewrites the ``vcenter`` variable query so its candidate values are filtered by
   the selected target type.

Why it does NOT touch panel queries
-----------------------------------
``type`` only exists as a label on ``vmware_target_info``. The business metrics
(``vmware_host_*``, ``vmware_vm_*``, ``vmware_datastore_*``, ...) deliberately do
not carry it -- stamping the target type onto every series would be pure
duplication, and it would change the identity of every existing time series.

Appending ``type=~"$target_type"`` to panel expressions would therefore match
nothing and blank out every panel. Instead the filter is applied one level up: the
``vcenter`` variable is derived from ``vmware_target_info``, so selecting a target
type narrows ``$vcenter``, and every panel already filters on ``vcenter=~"$vcenter"``.
The filter propagates for free and not a single panel query has to change.

Usage
-----
    python3 scripts/patch_dashboards.py            # patch in place (creates .bak)
    python3 scripts/patch_dashboards.py --check     # report only, exit 1 if unpatched
    python3 scripts/patch_dashboards.py --revert    # restore from .bak
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
DASHBOARD_DIR = REPO_ROOT / "dashboards"

VAR_NAME = "target_type"

# label_values on an info metric returns the empty set when no series match, which
# in Grafana renders as "None" rather than an error -- acceptable for a filter that
# defaults to matching everything.
TARGET_TYPE_QUERY = 'label_values(vmware_target_info{job="$job"}, type)'

# includeAll + allValue=".*" means the default view is unfiltered, so an existing
# dashboard behaves exactly as before until the user actively picks a type.
TARGET_TYPE_VAR = {
    "allValue": ".*",
    "current": {"selected": True, "text": "All", "value": "$__all"},
    "datasource": {"type": "prometheus", "uid": "${datasource}"},
    "definition": TARGET_TYPE_QUERY,
    "description": "Filter by scrape target type: vcenter or esxi.",
    "includeAll": True,
    "label": "Target Type",
    "multi": True,
    "name": VAR_NAME,
    "options": [],
    "query": {"query": TARGET_TYPE_QUERY, "refId": "StandardVariableQuery"},
    "refresh": 2,
    "regex": "",
    "sort": 1,
    "type": "query",
}

# The vcenter variable moves from vmware_vcenter_info to vmware_target_info so that
# the type filter can apply. Both metrics are emitted for every target, but only
# vmware_target_info carries `type`. Note the label rename: vmware_target_info uses
# `target`, so label_values() must ask for `target` while the variable keeps its
# `vcenter` name (renaming it would break every panel and every saved URL).
VCENTER_QUERY_OLD = 'label_values(vmware_vcenter_info{job="$job"},vcenter)'
VCENTER_QUERY_NEW = (
    'label_values(vmware_target_info{job="$job", type=~"$target_type"}, target)'
)


def load(path: pathlib.Path) -> dict:
    with path.open(encoding="utf-8") as fh:
        return json.load(fh)


def detect_indent(path: pathlib.Path) -> int:
    """Return the smallest indentation step the file uses.

    The bundled dashboards are not consistently formatted, so the width is detected
    per file rather than hardcoded. Taking the *minimum* observed indent matters:
    vmware-vm-view.json indents its top-level keys by 4 while still stepping by 2,
    so reading only the first indented line would guess 4 and reformat the whole
    file. Re-serialising thousands of untouched lines buries the real change in
    noise, which is exactly what this script exists to avoid.
    """
    widths = set()
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            stripped = line.lstrip(" ")
            if stripped != line and stripped.strip():
                widths.add(len(line) - len(stripped))
    return min(widths) if widths else 2


def dump(path: pathlib.Path, data: dict, indent: int) -> None:
    with path.open("w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=indent, ensure_ascii=False, sort_keys=False)
        fh.write("\n")


def find_var(variables: list[dict], name: str) -> tuple[int, dict] | tuple[None, None]:
    for idx, var in enumerate(variables):
        if var.get("name") == name:
            return idx, var
    return None, None


def patch(path: pathlib.Path) -> list[str]:
    """Patch one dashboard. Returns a list of human-readable changes."""
    indent = detect_indent(path)
    data = load(path)
    changes: list[str] = []

    variables = data.setdefault("templating", {}).setdefault("list", [])

    # 1. Insert the target_type variable right after `job`, so it appears before
    #    `vcenter` in the UI -- the filter chain reads job -> type -> vcenter.
    existing_idx, _ = find_var(variables, VAR_NAME)
    if existing_idx is None:
        job_idx, _ = find_var(variables, "job")
        insert_at = job_idx + 1 if job_idx is not None else 0
        variables.insert(insert_at, json.loads(json.dumps(TARGET_TYPE_VAR)))
        changes.append(f"added ${VAR_NAME} variable at index {insert_at}")

    # 2. Repoint the vcenter variable at vmware_target_info.
    _, vcenter = find_var(variables, "vcenter")
    if vcenter is None:
        changes.append("WARNING: no $vcenter variable found, filter will not apply")
    else:
        current = vcenter.get("definition", "")
        if current == VCENTER_QUERY_NEW:
            pass  # already patched
        elif current == VCENTER_QUERY_OLD:
            vcenter["definition"] = VCENTER_QUERY_NEW
            vcenter["query"] = {
                "query": VCENTER_QUERY_NEW,
                "refId": "StandardVariableQuery",
            }
            changes.append("repointed $vcenter at vmware_target_info")
        else:
            # Refuse to guess. An unexpected query means the dashboard diverged from
            # what this script was written against; silently rewriting it would be
            # worse than doing nothing.
            changes.append(
                f"WARNING: unexpected $vcenter query, left untouched: {current!r}"
            )

    if changes and not all(c.startswith("WARNING") for c in changes):
        backup = path.with_suffix(path.suffix + ".bak")
        if not backup.exists():
            backup.write_bytes(path.read_bytes())
        dump(path, data, indent)
        # Read back to prove we did not produce a broken file.
        verify = load(path)
        _, check = find_var(verify["templating"]["list"], VAR_NAME)
        if check is None:
            raise SystemExit(f"{path.name}: verification failed, {VAR_NAME} missing")

    return changes


def check(path: pathlib.Path) -> list[str]:
    data = load(path)
    variables = data.get("templating", {}).get("list", [])
    problems = []
    _, var = find_var(variables, VAR_NAME)
    if var is None:
        problems.append(f"missing ${VAR_NAME} variable")
    _, vcenter = find_var(variables, "vcenter")
    if vcenter is not None and vcenter.get("definition") != VCENTER_QUERY_NEW:
        problems.append("$vcenter not repointed at vmware_target_info")
    return problems


def revert(path: pathlib.Path) -> list[str]:
    backup = path.with_suffix(path.suffix + ".bak")
    if not backup.exists():
        return ["no backup found"]
    path.write_bytes(backup.read_bytes())
    backup.unlink()
    return ["restored from backup"]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--check", action="store_true", help="report only, do not write")
    group.add_argument("--revert", action="store_true", help="restore from .bak files")
    args = parser.parse_args()

    files = sorted(DASHBOARD_DIR.glob("*.json"))
    if not files:
        print(f"no dashboards found under {DASHBOARD_DIR}", file=sys.stderr)
        return 1

    failed = False
    for path in files:
        if args.check:
            problems = check(path)
            status = "OK" if not problems else "; ".join(problems)
            print(f"{path.name}: {status}")
            failed = failed or bool(problems)
        elif args.revert:
            for line in revert(path):
                print(f"{path.name}: {line}")
        else:
            changes = patch(path)
            if not changes:
                print(f"{path.name}: already patched, nothing to do")
            for line in changes:
                print(f"{path.name}: {line}")
                failed = failed or line.startswith("WARNING")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
