#!/usr/bin/env python3
"""Migrate the bundled Grafana dashboards onto the normalised metric names.

Why a script
------------
The five dashboards contain 161 PromQL expressions across 103 panels, 231 of
which reference a metric that was renamed. Hand-editing 80 KB of generated
Grafana JSON is how you get a dashboard that loads and renders wrong numbers.

Why renaming alone is not enough
--------------------------------
The dashboards do their own unit conversion, and the factors they hardcode are
exactly the ones the exporter now applies internally::

    vmware_host_cpu_capacity{...} * 1000 * 1000     # MHz -> Hz, by hand
    vmware_host_mem_consumed_average{...} * 1024    # kiloBytes -> bytes, by hand

``vmware_host_cpu_capacity_hertz`` already is hertz. Substituting the name and
leaving the ``* 1000 * 1000`` in place inflates the panel by 1e6 -- and Grafana
does not complain, it just draws the wrong number. So every rename has to be
paired with a decision about the adjacent factor.

How a factor is attributed to a metric
--------------------------------------
The parser matches ``metric{selector} * f1 * f2 ...`` as one unit: a numeric
factor chain **immediately following** a metric reference belongs to that
metric. A factor further away (``(a / b) * 100`` at the end of an expression)
belongs to the panel, not to any single metric, and is handled through
``PERCENT_PANELS`` instead.

This attribution matters because most expressions are ratios of two metrics with
*different* new factors::

    vmware_vm_mem_active_average / (vmware_vm_mem_capacity * 1024) * 100
                     ^ new factor 1024        ^ new factor 1048576   ^ panel

A regex that just rewrote every ``* 1024`` would have no way to tell which of
the two it belongs to.

What it changes
---------------
1. Metric names, from ``scripts/metric_migration_map.json`` (generated from the
   exporter's own ``translatePerfCounter``; see
   ``TestMigrationMapMatchesImplementation``).
2. Adjacent conversion factors, cancelled against the new factor. A factor that
   does not divide cleanly is **not guessed at** -- the script fails.
3. Panel ``unit`` fields, where the value's unit changed (``kbytes`` -> ``bytes``,
   ``percent`` -> ``percentunit``, ...).
4. Two genuine bugs found while doing the above, see ``BUG_FIXES``.

Usage
-----
    python3 scripts/migrate_dashboards.py            # migrate in place (.bak)
    python3 scripts/migrate_dashboards.py --check    # report only, exit 1 if stale
    python3 scripts/migrate_dashboards.py --revert   # restore from .bak
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
DASHBOARD_DIR = REPO_ROOT / "dashboards"
MAP_PATH = pathlib.Path(__file__).resolve().parent / "metric_migration_map.json"

# patch_dashboards.py already uses ".bak"; see migrate() for why this one differs.
BACKUP_SUFFIX = ".premigrate"


# ---------------------------------------------------------------------------
# Unit rewrites
# ---------------------------------------------------------------------------

# Grafana unit ids that describe a non-base unit, mapped to the base-unit id.
#
# The exporter used to emit kiloBytes and MHz, so the panels were configured to
# display kiloBytes and MHz. Now that the values are bytes and hertz, leaving the
# unit alone would relabel a correct number with the wrong suffix -- a 32 GiB host
# would read "32 GB" as "32 EB". Grafana has no way to notice.
UNIT_REWRITES = {
    "kbytes": "bytes",      # KiB -> B
    "mbytes": "bytes",      # MiB -> B
    "KiBs": "Bps",          # KiB/s -> B/s
    "ms": "s",              # milliseconds -> seconds
    "percent": "percentunit",  # 0..100 -> 0..1
}

# ``percent`` is the one unit whose rewrite depends on the expression rather than
# just the metric, so it is decided per panel by looking at the rewritten query.
#
# Most percent panels compute a ratio of two same-unit metrics and scale it
# themselves::
#
#     sum(mem_consumed_bytes) / sum(mem_capacity_bytes) * 100
#
# Renaming both sides does not change the range -- it is still 0..100, and the
# unit must stay ``percent``. Switching it to ``percentunit`` would divide the
# displayed value by 100 with nothing to indicate it.
#
# The unit only moves to ``percentunit`` when the * 100 is gone from the query,
# which happens in exactly two situations:
#
#   1. rate() replaced a "/ (20 * 1000) * 100" construct (ready, costop).
#   2. the metric itself became a ratio, so its "/ 100" was dropped (cpu latency).
#
# Deciding this from the rewritten expression rather than a hand-maintained list
# of panel titles means a new percent panel cannot be forgotten.
PERCENT_SCALE_RE = re.compile(r"[*/]\s*100\b")



# ---------------------------------------------------------------------------
# Explicit factor exceptions
# ---------------------------------------------------------------------------

# Adjacent factors that are neither "the exporter's conversion" nor a clean
# residual, keyed by (old metric name, factor as written).
#
# Every entry needs a reason. The point of listing them is that the script
# refuses to guess: an unrecognised combination stops the run instead of
# producing an expression that is off by some ratio, so this table is the only
# way a non-obvious case gets through.
#
# action:
#   "drop"  -- remove the factor, the new metric already carries the unit
#   "keep"  -- leave it alone, it is not a unit conversion at all
FACTOR_EXCEPTIONS: dict[tuple[str, float], tuple[str, str]] = {
    # (a - b) / a * 100 -- a percentage computed from two same-unit metrics.
    # Both sides keep factor 1 after the rename, so the * 100 still produces
    # 0..100 and the panel's `percent` unit stays correct. The factor lands
    # adjacent to the metric only because of where the parentheses close.
    ("vmware_datastore_capacity", 100.0): ("keep", "percentage arithmetic, not a unit conversion"),
    ("vmware_host_mem_capacity", 100.0): ("keep", "percentage arithmetic, not a unit conversion"),

    # vSphere percent counters are hundredths of a percentage point: 100 means
    # 1%. Dividing by 100 gave a percentage, which is what the panel's `percent`
    # unit expects. The new metric is a ratio (0..1), so the division goes away
    # and the unit becomes percentunit.
    ("vmware_host_cpu_latency_average", 0.01): ("drop", "old value was hundredths of a percent; new metric is a ratio"),
    ("vmware_vm_cpu_latency_average", 0.01): ("drop", "old value was hundredths of a percent; new metric is a ratio"),

    # mem.*.average was kiloBytes, mem_capacity was MB, so * 1024 brought the
    # capacity into kiloBytes to match the numerator. Both are bytes now.
    ("vmware_vm_mem_capacity", 1024.0): ("drop", "was MB -> kiloBytes to match a kiloBytes numerator; both are bytes now"),

    # disk.provisioned.latest (kiloBytes) / datastore_capacity (bytes) * 1024.
    # PromQL's * and / are left-associative and equal precedence, so this is
    # (provisioned / capacity) * 1024 -- the 1024 converts the numerator's
    # kiloBytes. Both are bytes now.
    ("vmware_datastore_capacity", 1024.0): ("drop", "cancelled the numerator's kiloBytes; both sides are bytes now"),

    # BUG: 8192 = 1024 * 8 is the kiloBytes -> bits conversion for throughput.
    # errors and dropped are packet counts. Multiplying a packet count by a
    # byte-to-bit factor is meaningless -- it inflated these series 8192x. This
    # is a copy-paste from the adjacent bytesRx/bytesTx panel.
    ("vmware_host_net_errorsRx_summation", 8192.0): ("drop", "BUG: packet counts must not be scaled by a bytes -> bits factor"),
    ("vmware_host_net_errorsTx_summation", 8192.0): ("drop", "BUG: packet counts must not be scaled by a bytes -> bits factor"),
    ("vmware_host_net_droppedRx_summation", 8192.0): ("drop", "BUG: packet counts must not be scaled by a bytes -> bits factor"),
    ("vmware_host_net_droppedTx_summation", 8192.0): ("drop", "BUG: packet counts must not be scaled by a bytes -> bits factor"),
}

# ---------------------------------------------------------------------------
# Bug fixes (not renames -- these change the rendered value on purpose)
# ---------------------------------------------------------------------------

# 1. net errors/dropped were multiplied by 8192.
#
#    8192 = 1024 * 8, the kiloBytes -> bits conversion that belongs on
#    net.bytesRx/bytesTx. errors and dropped are *packet counts*: multiplying
#    them by a byte-to-bit factor is meaningless and inflated those series by
#    8192x. It is a copy-paste from the adjacent throughput panel.
#
# 2. cpu ready/costop were divided by (20 * 1000).
#
#    20 is the value of -vmware.granularity, hardcoded into the dashboard. The
#    expression computed "accumulated milliseconds / sampling window in ms" to
#    get a ratio. The new metrics are proper counters in seconds, so the correct
#    form is rate(), which derives the window from the query step instead of
#    assuming it. Users who changed -vmware.granularity had these panels silently
#    wrong by the ratio of their setting to 20.
#
# Both are recorded in CHANGELOG.md as fixes rather than renames, because the
# numbers on screen change.
BUG_FIXES: list[dict] = []


def _bug_fix(pattern: str, replacement: str, why: str, expected: int) -> None:
    BUG_FIXES.append(
        {
            "re": re.compile(pattern, re.X),
            "to": replacement,
            "why": why,
            "expected": expected,
        }
    )


def load_map() -> dict[str, dict]:
    """Return {old_metric_name: entry} from the generated migration map."""
    with MAP_PATH.open(encoding="utf-8") as fh:
        entries = json.load(fh)

    by_old: dict[str, dict] = {}
    for entry in entries:
        old, new = entry["old"], entry["new"]
        previous = by_old.get(old)
        if previous is not None and previous["new"] != new:
            # Two subsystems disagreeing on the target name would make the
            # rewrite ambiguous. The generator sorts and dedupes, so this can
            # only happen if the file was hand-edited.
            raise SystemExit(
                f"{MAP_PATH.name}: {old} maps to both {previous['new']} and {new}"
            )
        by_old[old] = entry

    return by_old


# ---------------------------------------------------------------------------
# Expression rewriting
# ---------------------------------------------------------------------------

# metric{selector} followed by an optional chain of numeric factors.
#
# The selector is matched non-greedily up to the first '}' -- PromQL label
# selectors do not nest, so this is exact rather than approximate.
#
# The factor chain accepts newlines because several expressions are pretty-printed
# across multiple lines with the factor on its own line.
METRIC_RE = re.compile(
    r"""
    \b(?P<name>vmware_[A-Za-z0-9_]+)
    (?P<sel>\s*\{[^}]*\})?
    (?P<chain>(?:\s*[*/]\s*-?\s*\d+(?:\.\d+)?)*)
    """,
    re.X,
)

FACTOR_RE = re.compile(r"([*/])\s*(-?)\s*(\d+(?:\.\d+)?)")


def parse_chain(chain: str) -> tuple[float, bool]:
    """Return (multiplier, negated) for a factor chain like ' * 1024 * -8'.

    The sign is tracked separately from the magnitude. A leading minus in these
    dashboards is never a unit conversion -- it flips a series below the axis so
    receive and transmit can share one panel. Cancelling the magnitude must
    preserve the sign.
    """
    multiplier = 1.0
    negated = False

    for op, sign, digits in FACTOR_RE.findall(chain):
        value = float(digits)
        if sign == "-":
            negated = not negated
        multiplier = multiplier * value if op == "*" else multiplier / value

    return multiplier, negated


def format_chain(multiplier: float, negated: bool) -> str:
    """Render a residual multiplier back into PromQL, or '' if there is none."""
    if multiplier == 1.0 and not negated:
        return ""

    if multiplier == 1.0:
        return " * -1"

    # Integers print without a trailing '.0'; 8.0 as '* 8' not '* 8.0'.
    magnitude = int(multiplier) if float(multiplier).is_integer() else multiplier
    sign = "-" if negated else ""

    return f" * {sign}{magnitude}"


class Problem(Exception):
    """A rewrite the script refuses to guess at."""


def rewrite_expr(expr: str, by_old: dict[str, dict], where: str) -> tuple[str, list[str]]:
    """Rename metrics in one expression and cancel adjacent factors.

    Returns (new_expr, notes). Raises Problem when a factor cannot be cancelled
    cleanly, rather than emitting an expression that is off by some ratio.
    """
    notes: list[str] = []

    def repl(match: re.Match) -> str:
        name = match.group("name")
        sel = match.group("sel") or ""
        chain = match.group("chain") or ""

        entry = by_old.get(name)
        if entry is None:
            # Not renamed. Leave the whole match, factors included, untouched.
            return match.group(0)

        new_name = entry["new"]
        new_factor = float(entry["factor"])

        written, negated = parse_chain(chain)

        # No hardcoded factor: the panel was reading the raw value and relying on
        # its `unit` field to label it. The new metric is already in base units,
        # so no factor is needed here either -- only the unit needs updating,
        # which happens in migrate_panel(). Do not divide by new_factor: the
        # dashboard never applied a conversion, so there is nothing to cancel.
        if written == 1.0 and not negated:
            notes.append(f"{name} -> {new_name}")
            return new_name + sel

        # A hardcoded factor equal to the exporter's factor is exactly the
        # conversion the exporter now does internally. Drop it.
        if written == new_factor:
            notes.append(f"{name} -> {new_name} (dropped the redundant * {written:g})")
            return new_name + sel + format_chain(1.0, negated)

        # Consult the explicit exception table before falling back to residual
        # arithmetic. These are the cases where the adjacent factor is not the
        # exporter's conversion at all, so dividing by new_factor is meaningless.
        exception = FACTOR_EXCEPTIONS.get((name, written))
        if exception is not None:
            action, reason = exception
            if action == "keep":
                notes.append(f"{name} -> {new_name} (kept * {written:g}: {reason})")
                return new_name + sel + chain
            if action == "drop":
                notes.append(f"{name} -> {new_name} (dropped * {written:g}: {reason})")
                return new_name + sel + format_chain(1.0, negated)
            raise Problem(f"{where}: unknown action {action!r} for ({name}, {written:g})")

        residual = written / new_factor

        # Only residuals that correspond to a real, intentional conversion are
        # accepted. Everything else means the hardcoded factor and the exporter's
        # factor disagree in a way this script was not written for -- emitting a
        # silently-wrong expression is worse than stopping.
        #
        # 8 is bytes -> bits, the one legitimate residual in these dashboards:
        # the throughput panels hardcode 8192 = 1024 (kiloBytes -> bytes, now done
        # by the exporter) * 8 (bytes -> bits, still the panel's job).
        if residual != 8.0:
            raise Problem(
                f"{where}: {name} hardcodes a factor of {written:g} but the exporter "
                f"applies {new_factor:g}, leaving a residual of {residual:g}. "
                f"Only 8 (bytes -> bits) is a recognised residual. Fix the "
                f"expression by hand or teach the script about it."
            )

        notes.append(
            f"{name} -> {new_name} "
            f"({written:g} / {new_factor:g} = {residual:g}, bytes -> bits)"
        )

        return new_name + sel + format_chain(residual, negated)

    return METRIC_RE.sub(repl, expr), notes


# ---------------------------------------------------------------------------
# Semantic rewrites: counters must be read with rate()
# ---------------------------------------------------------------------------

# The delta counters used to be divided by the sampling window to get a ratio::
#
#     vmware_host_cpu_ready_summation{...} / (20 * 1000) * 100
#
# 20 is the default of -vmware.granularity and 1000 converts ms to s, so this
# reads "accumulated ready milliseconds / window length in ms" -- a ratio, then
# a percentage. It has two problems:
#
#   1. The granularity is hardcoded. A user running -vmware.granularity=300 got a
#      number 15x too small, with nothing to indicate it.
#   2. The old *_summation series was itself wrong, because the exporter averaged
#      the samples in the window instead of summing them (see CHANGELOG). So the
#      input to this division was already off whenever more than one sample
#      landed in a scrape interval.
#
# The new metric is a proper counter in seconds, so rate() gives the ratio
# directly: seconds accumulated per second elapsed. The window comes from
# $__rate_interval, which Grafana derives from the panel's step and the scrape
# interval -- no hardcoded assumption. The trailing * 100 goes away too, since
# rate() already yields 0..1 and the panel switches to percentunit.
#
# These 8 expressions are the reason this migration could not be verified by
# "the numbers before and after must match": the old numbers were wrong, so
# preserving them would be preserving a bug.
RATE_REWRITES = [
    # (old metric, new metric) -- the surrounding "/ (N * 1000)" and "* 100" are
    # matched and removed by RATE_DIVISOR_RE / TRAILING_PERCENT_RE below.
    ("vmware_host_cpu_ready_seconds_total", 3),
    ("vmware_host_cpu_costop_seconds_total", 3),
    ("vmware_vm_cpu_ready_seconds_total", 1),
    ("vmware_vm_cpu_costop_seconds_total", 1),
]

# Matches the hardcoded window divisor, with or without the parentheses and
# allowing whitespace or newlines anywhere.
RATE_DIVISOR_RE = re.compile(r"\s*/\s*\(\s*\d+\s*\*\s*1000\s*\)")

# Matches a trailing "* 100" at the very end of an expression, possibly after a
# closing paren and whitespace.
TRAILING_PERCENT_RE = re.compile(r"\s*\*\s*100\s*$")


def apply_rate_rewrites(expr: str) -> tuple[str, list[str]]:
    """Wrap counter references in rate() and strip the hardcoded window divisor.

    Runs after rewrite_expr(), so the metric names are already the new ones.
    """
    notes: list[str] = []

    counters = [name for name, _ in RATE_REWRITES if name in expr]
    if not counters:
        return expr, notes

    if not RATE_DIVISOR_RE.search(expr):
        # A counter reference without the divisor is either already migrated or
        # something this script has not seen. Either way, do not touch it: an
        # unwrapped counter is visibly broken in Grafana, a half-rewritten one
        # is not.
        return expr, notes

    result = RATE_DIVISOR_RE.sub("", expr)

    for name in counters:
        # Wrap the metric together with its selector. The selector is required
        # here -- rate() over a bare metric name would drop the $vcenter filter.
        pattern = re.compile(re.escape(name) + r"(\s*\{[^}]*\})")
        result, count = pattern.subn(
            lambda m: f"rate({name}{m.group(1)}[$__rate_interval])", result
        )
        if count:
            notes.append(f"{name} wrapped in rate() over $__rate_interval ({count}x)")

    stripped = TRAILING_PERCENT_RE.sub("", result)
    if stripped != result:
        notes.append("dropped the trailing * 100; rate() yields a ratio (unit -> percentunit)")
        result = stripped

    return result, notes



# ---------------------------------------------------------------------------
# Expression-level factors
# ---------------------------------------------------------------------------

# Factors that apply to a whole expression rather than to one metric reference.
#
# The adjacent-factor parser deliberately only claims a factor that directly
# follows a metric reference. These eleven expressions put the factor somewhere
# else::
#
#     sum(cpu_capacity{...} * cpu_corecount{...} * 1000 * 1000)   # after another metric
#     topk(..., max_over_time((...)[$__range:])) * 1000 * 1000    # outside a subquery
#     ((a * b) - (c)) * 1000 * 1000                               # applied to a group
#     sum(vm_mem_capacity{...} * on (vmmo) group_left vm_info{...} * 1024 * 1024)
#
# Widening the parser to reach them would mean attributing a factor to whichever
# metric happens to precede it, which is wrong as often as it is right --
# ``cpu_corecount`` and ``vm_info`` are dimensionless multipliers, not the owner
# of the conversion.
#
# So they are listed. Each entry is (factor as written, replacement, reason), and
# the factor is matched literally at the point it appears. In every case the
# expression's dimensioned metrics all share the same new factor, which is what
# makes a single expression-level cancellation correct:
#
#   * 1000 * 1000   -> cpu_capacity_hertz and cpu_usage_hertz are both 1e6
#   * 1024 * 1024   -> vm_mem_capacity_bytes is 1048576
#   * 8192          -> net_*_bytes_per_second is 1024, leaving * 8 for bytes -> bits
#
# ``suspicious_factors()`` fails the run if any of these survive, so a new
# expression of this shape cannot slip through unlisted.
#
# Each entry is (factor as written, replacement, reason, guard). The guard is a
# regex that must match the expression for the rule to fire, or None to always
# fire. It exists for factors whose meaning depends on context -- see the "/ 100"
# entry.
LATENCY_RATIO_RE = re.compile(r"vmware_(?:host|vm)_cpu_latency_ratio\b")

EXPRESSION_FACTORS = [
    (
        "* 1000 * 1000",
        "",
        "cpu capacity and usage are both hertz now; this was the MHz -> Hz conversion",
        None,
    ),
    (
        "* 1024 * 1024",
        "",
        "vm_mem_capacity_bytes is already bytes; this was the MB -> bytes conversion",
        None,
    ),
    (
        "* 8192",
        "* 8",
        "8192 = 1024 (kiloBytes -> bytes, now done by the exporter) * 8 (bytes -> bits, still needed)",
        None,
    ),
    # cluster and vcenter wrap the latency counter in avg() before dividing::
    #
    #     avg( vmware_host_cpu_latency_average{...} ) / 100
    #
    # so the / 100 is not adjacent to the metric and FACTOR_EXCEPTIONS never sees
    # it. host and vm write it without the avg(), where the adjacent-factor parser
    # does drop it. Left alone, the same metric would end up scaled differently
    # depending on which dashboard you opened -- and the two that kept the / 100
    # would divide an already-normalised ratio by another 100.
    #
    # All five "/ 100" in the bundled dashboards divide a latency counter, so this
    # entry is guarded on a *_latency_ratio reference being present rather than
    # matching "/ 100" anywhere: a percentage computed as "part / whole / 100"
    # would otherwise be silently rescaled. The check below fails the run if a
    # "/ 100" survives, so an unguarded one cannot pass unnoticed either.
    (
        "/ 100",
        "",
        "old percent counters were hundredths of a percent; *_latency_ratio is already a ratio",
        LATENCY_RATIO_RE,
    ),
]

# Normalised whitespace form -> compiled matcher, so "* 1000 * 1000" also matches
# "*1000*1000" and a version split across lines.
EXPRESSION_FACTOR_MATCHERS = [
    (
        re.compile(r"\s*" + r"\s*".join(re.escape(part) for part in written.split()) + r"(?!\d)"),
        replacement,
        reason,
        guard,
    )
    for written, replacement, reason, guard in EXPRESSION_FACTORS
]


def apply_expression_factors(expr: str) -> tuple[str, list[str]]:
    """Cancel a factor that applies to the expression rather than one metric."""
    notes: list[str] = []
    result = expr

    for matcher, replacement, reason, guard in EXPRESSION_FACTOR_MATCHERS:
        if guard is not None and not guard.search(result):
            continue
        replaced, count = matcher.subn(
            (" " + replacement) if replacement else "", result
        )
        if not count:
            continue
        result = replaced
        notes.append(
            f"expression-level factor cancelled {count}x"
            + (f", left {replacement}" if replacement else "")
            + f": {reason}"
        )

    return result, notes

# ---------------------------------------------------------------------------
# Panel and dashboard traversal
# ---------------------------------------------------------------------------


def iter_panels(node: dict, trail: str = ""):
    """Yield (title_path, panel) for every panel, including nested rows."""
    for panel in node.get("panels") or []:
        title = f"{trail}/{panel.get('title', '?')}"
        yield title, panel
        yield from iter_panels(panel, title)


def panel_unit(panel: dict) -> str | None:
    defaults = (panel.get("fieldConfig") or {}).get("defaults") or {}
    return defaults.get("unit")


def set_panel_unit(panel: dict, unit: str) -> None:
    panel.setdefault("fieldConfig", {}).setdefault("defaults", {})["unit"] = unit


# ---------------------------------------------------------------------------
# Per-panel range consistency
# ---------------------------------------------------------------------------

# A panel has one unit but many queries, so every query in it must produce the
# same range. Four "CPU Usage" panels break that after the rewrite:
#
#   Usage    ... / ... * 100                      -> 0..100, unchanged
#   Latency  vmware_host_cpu_latency_ratio        -> 0..1, was 0..100
#   Ready    rate(..._ready_seconds_total[...])   -> 0..1, was 0..100
#   Costop   rate(..._costop_seconds_total[...])  -> 0..1, was 0..100
#
# The Usage query is a ratio of two metrics that scales itself, so nothing in the
# rename touches its * 100 -- it still yields 0..100 and there is no factor to
# cancel. The other three became ratios. Under a single `percent` unit the three
# ratios would render 100x too small; under `percentunit` the Usage series would
# render 100x too large.
#
# Both fixes are defensible, so the tie-breaker is the rest of the panel: these
# panels set axisSoftMin/axisSoftMax to 0/100 and colour by percentage
# thresholds, all of which assume 0..100. Rescaling the three ratios back up
# keeps every one of those settings correct and keeps the panel reading exactly
# as it did before the migration. Switching the panel to percentunit would mean
# editing the axis bounds and thresholds too, for no visible gain.
#
# So: when a panel mixes ranges, the ratio-valued queries get an explicit * 100
# and the unit stays `percent`.
RATIO_VALUED_RE = re.compile(
    r"rate\(vmware_(?:host|vm)_cpu_(?:ready|costop)_seconds_total"
    r"|vmware_(?:host|vm)_cpu_latency_ratio\b"
)


def rescale_ratio_target(expr: str) -> str:
    """Scale a 0..1 expression back to 0..100 for a panel that stays `percent`."""
    body = expr.strip()
    # Keep the original layout readable: these expressions are multi-line, and
    # appending to the last line would bury the * 100 mid-indentation.
    if "\n" in expr:
        return f"(\n{expr}\n) * 100"
    return f"({body}) * 100"


def iter_target_exprs(panel: dict):
    for index, target in enumerate(panel.get("targets") or []):
        if not isinstance(target, dict):
            continue
        expr = target.get("expr")
        if isinstance(expr, str) and expr:
            yield index, target, expr


def unify_panel_range(panel: dict) -> list[str]:
    """Make every query in a `percent` panel agree on 0..100.

    Only fires when the panel genuinely mixes ranges -- if all queries became
    ratios, the panel switches to percentunit instead (handled by the caller).
    """
    notes: list[str] = []

    scaled = []
    ratios = []
    for index, target, expr in iter_target_exprs(panel):
        if PERCENT_SCALE_RE.search(expr):
            scaled.append(index)
        elif RATIO_VALUED_RE.search(expr):
            ratios.append((index, target, expr))

    if not scaled or not ratios:
        return notes

    for index, target, expr in ratios:
        target["expr"] = rescale_ratio_target(expr)
        notes.append(
            f"target[{index}]: scaled back to 0..100 with * 100 so it matches the "
            f"other queries in this panel (unit stays percent)"
        )

    return notes


def rescale_panel_bounds(panel: dict) -> list[str]:
    """Rescale axis bounds and thresholds when a panel moves to percentunit.

    The unit change alters what the numbers mean, and Grafana compares thresholds
    and axis bounds against the raw value, not the formatted one. vm CPU Latency
    sets max=100 and colours at 60/80: left alone, a ratio of 0.03 would sit on a
    0..100 axis as a flat line at the bottom and never leave the green band, so
    the panel would look healthy no matter how bad latency got.

    Only absolute thresholds are touched. `percentage` mode is already relative to
    the axis range, so its steps stay valid.
    """
    notes: list[str] = []
    defaults = (panel.get("fieldConfig") or {}).get("defaults") or {}

    for key in ("min", "max"):
        value = defaults.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool) and value:
            defaults[key] = value / 100
            notes.append(f"{key} {value} -> {defaults[key]} (0..100 -> 0..1)")

    custom = defaults.get("custom") or {}
    for key in ("axisSoftMin", "axisSoftMax"):
        value = custom.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool) and value:
            custom[key] = value / 100
            notes.append(f"{key} {value} -> {custom[key]} (0..100 -> 0..1)")

    thresholds = defaults.get("thresholds") or {}
    if thresholds.get("mode") == "absolute":
        for step in thresholds.get("steps") or []:
            value = step.get("value")
            if isinstance(value, (int, float)) and not isinstance(value, bool) and value:
                step["value"] = value / 100
                notes.append(
                    f"threshold {value} -> {step['value']} ({step.get('color')})"
                )

    return notes


def migrate_panel(
    stem: str, title: str, panel: dict, by_old: dict[str, dict]
) -> list[str]:
    """Rewrite one panel's targets and unit. Returns human-readable changes."""
    changes: list[str] = []
    targets = panel.get("targets") or []

    rate_applied = False

    for index, target in enumerate(targets):
        if not isinstance(target, dict):
            continue

        expr = target.get("expr")
        if not isinstance(expr, str) or not expr:
            continue

        where = f"{stem}{title} target[{index}]"

        renamed, notes = rewrite_expr(expr, by_old, where)
        rated, rate_notes = apply_rate_rewrites(renamed)
        final, factor_notes = apply_expression_factors(rated)

        if final == expr:
            continue

        target["expr"] = final
        rate_applied = rate_applied or bool(rate_notes)

        for note in notes + rate_notes + factor_notes:
            changes.append(f"{title} target[{index}]: {note}")

    if not changes:
        return changes

    unit = panel_unit(panel)
    if unit not in UNIT_REWRITES:
        return changes

    if unit == "percent":
        # Decide from what the queries produce *now*, after the rewrite.
        for note in unify_panel_range(panel):
            changes.append(f"{title} {note}")

        still_scaled = any(
            PERCENT_SCALE_RE.search(expr) for _, _, expr in iter_target_exprs(panel)
        )
        if still_scaled:
            # The panel still scales to 0..100 itself, so `percent` is correct.
            return changes

        set_panel_unit(panel, "percentunit")
        reason = "rate() yields a ratio" if rate_applied else "the metric is now a ratio"
        changes.append(f"{title}: unit percent -> percentunit ({reason})")
        changes.extend(f"{title} {note}" for note in rescale_panel_bounds(panel))
        return changes

    new_unit = UNIT_REWRITES[unit]
    set_panel_unit(panel, new_unit)
    changes.append(f"{title}: unit {unit} -> {new_unit}")

    return changes


# ---------------------------------------------------------------------------
# File I/O
# ---------------------------------------------------------------------------


def detect_indent(path: pathlib.Path) -> int:
    """Return the smallest indentation step the file uses.

    Same approach as patch_dashboards.py: the bundled dashboards are not
    consistently formatted, and re-serialising thousands of untouched lines would
    bury the real change in noise. Taking the minimum matters because
    vmware-vm-view.json indents top-level keys by 4 while stepping by 2.
    """
    widths = set()
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            stripped = line.lstrip(" ")
            if stripped != line and stripped.strip():
                widths.add(len(line) - len(stripped))
    return min(widths) if widths else 2


def load(path: pathlib.Path) -> dict:
    with path.open(encoding="utf-8") as fh:
        return json.load(fh)


def dump(path: pathlib.Path, data: dict, indent: int) -> None:
    with path.open("w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=indent, ensure_ascii=False, sort_keys=False)
        fh.write("\n")


def migrate(path: pathlib.Path, by_old: dict[str, dict]) -> list[str]:
    indent = detect_indent(path)
    data = load(path)
    stem = path.stem

    changes: list[str] = []
    for title, panel in iter_panels(data):
        changes.extend(migrate_panel(stem, title, panel, by_old))

    if not changes:
        return changes

    # A distinct suffix from patch_dashboards.py's ".bak" on purpose. Sharing it
    # would make this script skip the backup (the file already exists) and then
    # make --revert restore a state from before the target_type patch -- silently
    # undoing someone else's change.
    backup = path.with_suffix(path.suffix + BACKUP_SUFFIX)
    if not backup.exists():
        backup.write_bytes(path.read_bytes())

    dump(path, data, indent)

    # Read back and prove no old metric name survived. A rewrite that silently
    # missed a reference is the failure mode that matters here: the panel loads
    # and renders nothing, with no error anywhere.
    verify = load(path)
    leftovers = stale_names(verify, by_old)
    if leftovers:
        raise SystemExit(
            f"{path.name}: verification failed, these old names survived the "
            f"rewrite: {sorted(leftovers)}"
        )

    residue = suspicious_factors(verify)
    if residue:
        detail = "\n      ".join(" ".join(expr.split()) for expr in residue)
        raise SystemExit(
            f"{path.name}: verification failed, a unit-conversion factor survived "
            f"the rewrite. The exporter now applies it internally, so leaving it "
            f"in place multiplies the panel by that factor:\n      {detail}"
        )

    rescaled = stale_latency_scale(verify)
    if rescaled:
        detail = "\n      ".join(" ".join(expr.split()) for expr in rescaled)
        raise SystemExit(
            f"{path.name}: verification failed, a '/ 100' survived next to a "
            f"*_latency_ratio reference. The metric is already a ratio, so this "
            f"divides it a second time and understates latency 100x:"
            f"\n      {detail}"
        )

    mixed = mixed_range_panels(verify)
    if mixed:
        detail = "\n      ".join(mixed)
        raise SystemExit(
            f"{path.name}: verification failed, a panel mixes 0..1 and 0..100 "
            f"queries under a single unit, so one of them renders 100x off:"
            f"\n      {detail}"
        )

    return changes


def iter_exprs(node) -> list[str]:
    out: list[str] = []

    def walk(current) -> None:
        if isinstance(current, dict):
            for key, value in current.items():
                if key == "expr" and isinstance(value, str):
                    out.append(value)
                else:
                    walk(value)
        elif isinstance(current, list):
            for value in current:
                walk(value)

    walk(node)
    return out


# Conversion factors that must not survive the migration anywhere in an
# expression, because the exporter now applies them itself.
#
# The adjacent-factor parser only sees factors directly after a metric reference.
# One expression puts the factor at the end of a group_left join::
#
#     sum(vmware_vm_mem_capacity{...} * on (vmmo) group_left vmware_vm_info{...} * 1024 * 1024)
#
# The 1024 * 1024 belongs to mem_capacity but is nowhere near it. Rather than
# widen the parser -- which would start attributing factors to whatever metric
# happens to precede them -- the script scans for leftovers and refuses to
# finish. A missed factor is exactly the failure this script exists to prevent,
# and it is invisible in Grafana.
SUSPICIOUS_FACTOR_RE = re.compile(
    r"[*/]\s*(?:1024|8192|1000\s*\*\s*1000|1048576)\b"
)

# A "/ 100" left next to a *_latency_ratio reference means the migration missed
# one: the metric is already a ratio, so dividing again understates latency by
# 100x. Checked separately from the factors above because "/ 100" on its own is
# legitimate elsewhere -- it is only wrong in the company of a ratio metric.
DIV_100_RE = re.compile(r"/\s*100(?!\d)")


def stale_latency_scale(data: dict) -> list[str]:
    """Return expressions that still divide an already-normalised ratio by 100."""
    return [
        expr
        for expr in iter_exprs(data)
        if LATENCY_RATIO_RE.search(expr) and DIV_100_RE.search(expr)
    ]


def suspicious_factors(data: dict) -> list[str]:
    """Return expressions that still contain a unit-conversion factor."""
    return [
        expr for expr in iter_exprs(data) if SUSPICIOUS_FACTOR_RE.search(expr)
    ]


def stale_names(data: dict, by_old: dict[str, dict]) -> set[str]:
    """Return the old metric names still referenced anywhere in the dashboard."""
    found: set[str] = set()
    for expr in iter_exprs(data):
        for name in re.findall(r"\bvmware_[A-Za-z0-9_]+", expr):
            if name in by_old:
                found.add(name)
    return found


def mixed_range_panels(data: dict) -> list[str]:
    """Return panels whose queries disagree about their range.

    One panel means one unit, so a panel holding both a 0..100 query and a 0..1
    query has to render one of them wrong. Grafana gives no hint: both draw fine,
    one is just off by two orders of magnitude. Hidden targets count -- unhiding
    one in the UI must not silently produce a wrong line.
    """
    problems: list[str] = []

    for title, panel in iter_panels(data):
        if panel_unit(panel) not in ("percent", "percentunit"):
            continue

        scaled: list[int] = []
        ratios: list[int] = []
        for index, _, expr in iter_target_exprs(panel):
            if PERCENT_SCALE_RE.search(expr):
                scaled.append(index)
            elif RATIO_VALUED_RE.search(expr):
                ratios.append(index)

        if scaled and ratios:
            problems.append(
                f"{title} (unit={panel_unit(panel)}): targets {scaled} yield 0..100 "
                f"but targets {ratios} yield 0..1"
            )

    return problems


def check(path: pathlib.Path, by_old: dict[str, dict]) -> list[str]:
    """Report anything wrong with an already-migrated dashboard.

    Runs the same assertions the migration verifies itself against, so this is
    what CI should call: it catches a hand-edit that reintroduces an old name, a
    stale factor, or a panel whose queries disagree about their range.
    """
    data = load(path)
    problems: list[str] = []

    stale = stale_names(data, by_old)
    if stale:
        problems.append(
            f"still references {len(stale)} pre-rename metric(s): {sorted(stale)}"
        )

    for expr in suspicious_factors(data):
        problems.append(
            f"unit-conversion factor still applied by the dashboard: "
            f"{' '.join(expr.split())}"
        )

    for expr in stale_latency_scale(data):
        problems.append(
            f"'/ 100' applied to an already-normalised ratio: "
            f"{' '.join(expr.split())}"
        )

    problems.extend(mixed_range_panels(data))

    return problems


def revert(path: pathlib.Path) -> list[str]:
    backup = path.with_suffix(path.suffix + BACKUP_SUFFIX)
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
    parser.add_argument(
        "--verbose", action="store_true", help="list every individual rewrite"
    )
    args = parser.parse_args()

    by_old = load_map()

    files = sorted(p for p in DASHBOARD_DIR.glob("*.json"))
    if not files:
        print(f"no dashboards found under {DASHBOARD_DIR}", file=sys.stderr)
        return 1

    failed = False
    for path in files:
        if args.check:
            problems = check(path, by_old)
            print(f"{path.name}: {'OK' if not problems else '; '.join(problems)}")
            failed = failed or bool(problems)
            continue

        if args.revert:
            for line in revert(path):
                print(f"{path.name}: {line}")
            continue

        try:
            changes = migrate(path, by_old)
        except Problem as problem:
            print(f"{path.name}: {problem}", file=sys.stderr)
            failed = True
            continue

        if not changes:
            print(f"{path.name}: already migrated, nothing to do")
            continue

        print(f"{path.name}: {len(changes)} change(s)")
        if args.verbose:
            for line in changes:
                print(f"    {line}")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
