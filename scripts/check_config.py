#!/usr/bin/env python3
"""Guard against credentials and unreachable env vars in shipped config.

Run this in CI:

    python3 scripts/check_config.py

Three classes of problem are caught, in increasing order of subtlety, plus a
documentation check:

1. **Credential regressions.** Real passwords and internal addresses used to
   live in docker-compose.yml, vmware.conf and README-zh.md. They are in the
   git history and cannot be removed from it (the repo is a fork, so rewriting
   history would break the relationship), which makes it all the more important
   that no new one gets added. Every text file tracked by git is scanned, not a
   curated list of them -- see LEAK_SCAN_EXCEPTIONS for why.

2. **Credentials on the command line.** Anything in a compose `command:`, or on
   the systemd unit's `ExecStart`, ends up in the process cmdline -- readable via
   `docker inspect`, via `ps`, and via /proc. Moving the password to
   `environment:` / `EnvironmentFile` plus `-envflag.enable` keeps it out.

3. **Environment variables that map to no flag.** This is the one worth
   automating. envflag derives the variable name from the flag name -- prefix,
   then the flag name with dots replaced by underscores, *keeping the flag's
   original case*. `VMWARE_VMWARE_PASSWORD` looks right and is silently
   ignored: no warning, no error, the exporter just uses the flag default and
   fails to authenticate for a reason nothing in the logs explains. A name that
   derives no known flag is therefore a real bug, not a style issue.

   The list of valid flags comes from building the exporter and reading
   `--help`, not from grepping the source: the collector flags are constructed
   with `fmt.Sprintf("collector.%s", ...)` and a literal grep cannot see them.

4. **Flags missing from the READMEs.** A registered flag with no documentation
   entry is invisible to users. Found `-disable.exporter.metrics` and
   `-disable.exporter.target` undocumented in README-zh.md this way -- the
   first of which defaults to *true*, so the exporter's own `go_*` metrics are
   absent by default and nothing said so.

5. **Dependency ecosystems nobody is watching.** dependabot.yml declared only
   `gomod`, so the GitHub Actions had quietly drifted a major version behind --
   twice, because checking them by hand is exactly as reliable as it sounds.
   Every ecosystem the repository actually contains must be declared, so drift
   arrives as a pull request rather than as a broken release.

6. **Dashboards querying metrics that no longer exist.** The bundled dashboards
   were migrated to the renamed metrics, and the unit conversions the exporter
   now applies internally were removed from the queries. Both failure modes are
   silent in Grafana: a stale metric name draws an empty panel, and a leftover
   `* 1024` draws a number that is wrong by three orders of magnitude. The rules
   are reused from scripts/migrate_dashboards.py rather than duplicated.

7. **A systemd unit whose reload directive disagrees with the binary.** The rule
   here is bidirectional, because it has already been wrong in both directions.
   The exporter originally installed no signal handlers, so SIGHUP terminated the
   process and an `ExecReload` made `systemctl reload` report success while
   killing the service. It now installs a SIGHUP handler that re-reads `-file`
   and the environment, so the unit *should* carry `ExecReload` -- omitting it
   forces a restart and drops metrics for that window. The check reads
   vmware-exporter.go for `signal.Notify` + `syscall.SIGHUP` and requires the
   unit to match, so removing the handler makes `ExecReload` a failure again
   rather than silently restoring the original footgun.

Exit code is 0 when clean, 1 when any check fails, 2 on a missing dependency.
"""

from __future__ import annotations

import importlib.util
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

try:
    import yaml
except ImportError:
    print("error: pyyaml is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

COMPOSE = os.path.join(REPO, "docker-compose.yml")
SERVICE_NAME = "vmware-exporter"

# Values that were committed in plain text at some point. They stay in the git
# history; the point of listing them is to notice if one is reintroduced.
KNOWN_LEAKS = [
    "QOo%zF7AsvJ280.s@sc",
    "public@123",
    "public@12345",
    "public@54321",
    "172.17.41.101",
    "172.16.10.1",
    "172.16.10.10",
    "172.16.10.100",
]

# Files users copy from verbatim. A live credential in any of these is as bad
# as one in the compose file.
#
# This used to be a hand-maintained list of five paths, which had exactly the
# bug it was written to prevent: README-zh.md kept its plaintext passwords
# through the first pass of the security work precisely because it was not on
# somebody's list. A whitelist that misses a file fails silently; a scan that
# covers everything and carries explicit exceptions fails loudly. So: scan every
# text file git tracks, and name the exceptions here with a reason.
LEAK_SCAN_EXCEPTIONS = {
    # Documents which passwords leaked and when, so users know what to rotate.
    # Redacting it would defeat its purpose.
    "CHANGELOG.md",
    # Holds KNOWN_LEAKS itself.
    "scripts/check_config.py",
}

# Extensions worth reading as text. Go sources are excluded on purpose: they are
# covered by the compiler and tests, and test fixtures legitimately contain
# host-like strings.
TEXT_SUFFIXES = (
    ".md", ".yml", ".yaml", ".conf", ".service", ".json",
    ".sh", ".toml", ".ini", ".cfg", ".env", ".py", ".txt",
)
TEXT_NAMES = ("Dockerfile", "Makefile", "LICENSE")


def scanned_files() -> list[str]:
    """Every text file git tracks, minus the documented exceptions."""
    out = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=REPO,
        capture_output=True,
        text=True,
        check=True,
    )
    found = []
    for rel in out.stdout.split("\0"):
        if not rel or rel in LEAK_SCAN_EXCEPTIONS:
            continue
        base = os.path.basename(rel)
        if rel.endswith(TEXT_SUFFIXES) or base in TEXT_NAMES:
            found.append(rel)
    return sorted(found)


def flags_from_binary() -> set[str] | None:
    """Ask the exporter itself which flags exist. None if Go is unavailable.

    This is the authoritative answer, and the grep below is not, because the
    collector flags are built at registration time:

        flag.Bool(fmt.Sprintf("collector.%s", clusterSubsystem), ...)

    A literal-string grep cannot see `collector.cluster`, `collector.vm` and the
    rest, so it reports a *false positive* on a perfectly valid
    `VMWARE_collector_vm` -- turning a correct config into a red build. `--help`
    also covers flags registered by the exporter-toolkit and envflag packages,
    which live outside this repository entirely.
    """
    try:
        binary = os.path.join(tempfile.mkdtemp(prefix="flagprobe-"), "exporter")
        build = subprocess.run(
            ["go", "build", "-o", binary, "."],
            cwd=REPO,
            capture_output=True,
            text=True,
            check=False,
            env={**os.environ, "CGO_ENABLED": "0"},
        )
        if build.returncode != 0:
            return None
        # --help exits non-zero by convention; the output is what matters.
        out = subprocess.run(
            [binary, "--help"], capture_output=True, text=True, check=False
        )
        # `-` belongs in the character class: `-collector.max-concurrency` was
        # being captured as `collector.max`, which then failed to match its own
        # README row and silently mismatched every other check keyed on flag
        # names. Flags with hyphens are normal in this codebase.
        names = set(re.findall(r"^\s+-([a-zA-Z][\w.-]*)", out.stdout + out.stderr, re.M))
        return names or None
    except (OSError, subprocess.SubprocessError):
        return None
    finally:
        shutil.rmtree(os.path.dirname(binary), ignore_errors=True)


def flags_from_source() -> set[str]:
    """Fallback: literal flag names in the source. Misses constructed ones."""
    out = subprocess.run(
        ["git", "grep", "-hoE", r'flag\.[A-Za-z]+\(\s*"[^"]+"', "--", "*.go"],
        cwd=REPO,
        capture_output=True,
        text=True,
        check=False,
    )
    return set(re.findall(r'"([^"]+)"', out.stdout))


def registered_flags() -> tuple[set[str], str]:
    """Every flag the exporter registers, plus how it was determined."""
    from_binary = flags_from_binary()
    if from_binary:
        return from_binary, "binary"
    return flags_from_source(), "source"


def check_leaks(failures: list[str], files: list[str]) -> None:
    # A stale exception is a hole: if CHANGELOG.md were renamed, the entry here
    # would quietly stop excusing anything while still looking deliberate.
    for rel in sorted(LEAK_SCAN_EXCEPTIONS):
        if not os.path.exists(os.path.join(REPO, rel)):
            failures.append(
                f"LEAK_SCAN_EXCEPTIONS lists {rel!r}, which does not exist; "
                "remove the entry or fix the path"
            )

    for rel in files:
        path = os.path.join(REPO, rel)
        if not os.path.exists(path):
            continue
        try:
            text = open(path, encoding="utf-8").read()
        except (UnicodeDecodeError, OSError):
            continue
        for secret in KNOWN_LEAKS:
            if secret in text:
                failures.append(
                    f"{rel}: previously leaked value {secret!r} is present again"
                )


def check_readme_flags(failures: list[str], flags: set[str], source: str) -> None:
    """Every registered flag must have a row in a README reference table.

    Documentation drifts silently: a flag added without an entry is invisible to
    users. Only meaningful with the authoritative flag list, so it is skipped in
    the grep fallback rather than producing noise.

    Deliberately restricted to table rows. Matching prose mentions as well made
    the check useless: `-disable.exporter.metrics` was missing from
    README-zh.md's table while being discussed in the paragraph below it, and an
    earlier version of this function passed on that basis. A flag explained in
    passing but absent from the reference table is still undocumented as far as
    somebody scanning for options is concerned.
    """
    if source != "binary":
        return

    for doc in ("README.md", "README-zh.md"):
        path = os.path.join(REPO, doc)
        if not os.path.exists(path):
            continue
        text = open(path, encoding="utf-8").read()
        # A table row starts with `|`, then the flag, optionally in backticks:
        #   | -disable.exporter.metrics | ... |     (README.md)
        #   | `-vmware.vcenter` | string | ... |    (README-zh.md)
        #
        # The character class must include `-`: `-collector.max-concurrency`
        # was being truncated to `collector.max` by an earlier `[\w.]*`, so the
        # flag counted as undocumented no matter what the table said. Any flag
        # with a hyphen in its name had the same problem.
        documented = set(re.findall(r"^\|\s*`?-([a-zA-Z][\w.-]*)", text, re.M))
        for flag in sorted(flags - documented):
            failures.append(
                f"{doc}: flag -{flag} is registered but not documented"
            )

        # The reverse direction. Without it a renamed flag leaves its old name
        # in the table forever: the check above only notices the new name being
        # absent, and once you add that row the stale one stops being visible to
        # any check at all. `-prom.maxRequests` survived exactly this way -- it
        # had not existed in the binary for a while and both READMEs still
        # listed it with a default value.
        #
        # Stale documentation is worse than missing documentation: somebody
        # copies the flag into a systemd unit, the exporter refuses to start,
        # and the table they copied it from still says it is valid.
        for flag in sorted(documented - flags):
            failures.append(
                f"{doc}: flag -{flag} is documented but not registered"
            )


def check_dependabot(failures: list[str]) -> None:
    """Every ecosystem present in the repository must be declared to dependabot.

    Version drift is not caught by any of the checks above, and checking it by
    hand does not work: the GitHub Actions in this repository fell a major
    version behind twice, and both times a manual review had just declared them
    current. The fix is not to review harder, it is to make sure something is
    subscribed to each ecosystem -- which is a property of this file and can be
    verified offline.

    Deliberately does not query the network for latest versions. That belongs to
    dependabot, which has the credentials and the rate limits for it; duplicating
    it here would make the check flaky and the failure uninformative.
    """
    path = os.path.join(REPO, ".github", "dependabot.yml")
    if not os.path.exists(path):
        failures.append(
            ".github/dependabot.yml is missing, so no dependency updates are "
            "being proposed at all"
        )
        return

    try:
        doc = yaml.safe_load(open(path, encoding="utf-8")) or {}
    except yaml.YAMLError as exc:
        failures.append(f".github/dependabot.yml is not valid YAML: {exc}")
        return

    declared = {
        str(u.get("package-ecosystem"))
        for u in (doc.get("updates") or [])
        if isinstance(u, dict)
    }

    # What the repository actually contains, and the marker that proves it.
    present = {}
    if os.path.exists(os.path.join(REPO, "go.mod")):
        present["gomod"] = "go.mod"
    workflows = os.path.join(REPO, ".github", "workflows")
    if os.path.isdir(workflows) and os.listdir(workflows):
        present["github-actions"] = ".github/workflows/"
    if os.path.exists(os.path.join(REPO, "Dockerfile")):
        present["docker"] = "Dockerfile"

    for eco, marker in sorted(present.items()):
        if eco not in declared:
            failures.append(
                f".github/dependabot.yml: no `{eco}` entry, but {marker} exists; "
                "nothing is watching that ecosystem for updates"
            )


def check_compose(failures: list[str], flags: set[str]) -> None:
    text = open(COMPOSE, encoding="utf-8").read()
    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        failures.append(f"docker-compose.yml is not valid YAML: {exc}")
        return

    try:
        svc = doc["services"][SERVICE_NAME]
    except (KeyError, TypeError):
        failures.append(
            f"docker-compose.yml: services.{SERVICE_NAME} is not reachable "
            "(check the indentation of the service block)"
        )
        return

    for key in ("image", "ports", "environment", "command"):
        if key not in svc:
            failures.append(f"docker-compose.yml: services.{SERVICE_NAME} lacks `{key}`")

    command = [str(c) for c in (svc.get("command") or [])]

    for item in command:
        low = item.lower()
        if "password" in low or "username" in low:
            failures.append(
                f"docker-compose.yml: credential flag on the command line: {item!r}\n"
                "    the container cmdline is readable via docker inspect and /proc"
            )

    if "-envflag.enable" not in command:
        failures.append(
            "docker-compose.yml: command lacks -envflag.enable, so the "
            "`environment:` entries would be ignored entirely"
        )

    prefix = None
    for item in command:
        m = re.match(r"^-envflag\.prefix=(.*)$", item)
        if m:
            prefix = m.group(1)
    if prefix is None:
        failures.append("docker-compose.yml: command lacks -envflag.prefix")
        prefix = "VMWARE_"

    # Names declared in `environment:`, plus the ones written in the .env
    # example in the comments -- users copy those verbatim, so a typo there is
    # just as harmful as one in the YAML itself.
    names = [str(e).split("=", 1)[0] for e in (svc.get("environment") or [])]
    names += re.findall(rf"^#\s+({re.escape(prefix)}\S+?)=", text, re.MULTILINE)
    names += re.findall(rf"^#\s+-\s+({re.escape(prefix)}\S+)$", text, re.MULTILINE)

    for name in sorted(set(names)):
        if not name.startswith(prefix):
            failures.append(
                f"docker-compose.yml: env var {name!r} does not start with the "
                f"configured -envflag.prefix {prefix!r}"
            )
            continue
        derived = name[len(prefix):].replace("_", ".")
        if derived not in flags:
            failures.append(
                f"docker-compose.yml: env var {name!r} derives flag {derived!r}, "
                "which no flag registration matches.\n"
                "    envflag keeps the flag's original case, so it would ignore "
                "this variable without reporting anything."
            )


def check_conf(failures: list[str], flags: set[str]) -> None:
    """vmware.conf holds VARIABLE=value lines for the unit's EnvironmentFile.

    It used to hold a single `ARGS="-vmware.password=..."` line that the unit
    expanded onto ExecStart, which put the password into the process cmdline --
    readable by any user on the host via /proc/<pid>/cmdline or `ps`. That is the
    same leak docker-compose.yml was fixed for, so this file now uses the
    environment instead and the check moved with it.

    An ARGS= line is therefore treated as a regression, not just a stale style:
    it silently reintroduces the leak, and it does so while looking like a
    working configuration.
    """
    path = os.path.join(REPO, "vmware.conf")
    if not os.path.exists(path):
        return
    text = open(path, encoding="utf-8").read()

    body = [
        ln.strip() for ln in text.splitlines()
        if ln.strip() and not ln.strip().startswith("#")
    ]

    for ln in body:
        if ln.startswith("ARGS="):
            failures.append(
                "vmware.conf: ARGS= line found; the unit no longer expands it "
                "onto ExecStart.\n"
                "    Flags passed that way land in the process cmdline, where "
                "any user can read the password out of /proc.\n"
                "    Use VMWARE_<flag> variables instead -- see the header of "
                "the file."
            )

    # The unit is useless without a target, so these three must be present.
    # Without this, emptying the file would pass every other check here.
    assignments = {}
    for ln in body:
        if ln.startswith("ARGS=") or "=" not in ln:
            continue
        name, value = ln.split("=", 1)
        assignments[name.strip()] = value.strip()

    required = (
        "VMWARE_vmware_vcenter",
        "VMWARE_vmware_username",
        "VMWARE_vmware_password",
    )
    for name in required:
        if name not in assignments:
            failures.append(
                f"vmware.conf: {name} is not set; the shipped example must stay "
                "runnable after filling in the placeholders"
            )

    # Every variable, including the commented-out optional ones, must derive a
    # real flag. This is the check that actually earns its keep: envflag keeps
    # the flag's original case, so VMWARE_VMWARE_PASSWORD is accepted by the
    # file, ignored by the exporter, and reported by nothing.
    prefix = "VMWARE_"
    names = set(assignments)
    names |= set(re.findall(rf"^#\s*({re.escape(prefix)}\S+?)=", text, re.MULTILINE))

    for name in sorted(names):
        if not name.startswith(prefix):
            failures.append(
                f"vmware.conf: variable {name!r} does not start with {prefix!r}, "
                "so -envflag.prefix=VMWARE_ would never look at it"
            )
            continue
        derived = name[len(prefix):].replace("_", ".")
        if derived not in flags:
            failures.append(
                f"vmware.conf: variable {name!r} derives flag {derived!r}, which "
                "no flag registration matches.\n"
                "    envflag keeps the flag's original case, so it would ignore "
                "this variable without reporting anything."
            )

    # A placeholder must stay a placeholder.
    for name in ("VMWARE_vmware_password", "VMWARE_vmware_username"):
        value = assignments.get(name)
        if value is None:
            continue
        if not (value.startswith("<") and value.endswith(">")):
            failures.append(
                f"vmware.conf: {name} carries a literal value ({value!r}); "
                "it must stay a <PLACEHOLDER>"
            )


def check_unit(failures: list[str], flags: set[str]) -> None:
    """Assert the shipped systemd unit exposes reload and never leaks credentials.

    The reload rule here is INVERTED relative to an earlier version of this
    script, and the inversion is the point.

    Originally the exporter installed no signal handlers, so SIGHUP hit Go's
    default disposition and terminated the process -- `ExecReload=/bin/kill -HUP
    $MAINPID` made `systemctl reload` report success while killing the service.
    This check therefore rejected any ExecReload line.

    The binary now installs a SIGHUP handler (handleReloadSignals in
    vmware-exporter.go) that re-reads -file and the environment. So the unit
    SHOULD carry ExecReload, and the failure mode has flipped: a unit without it
    forces operators into a restart, which drops metrics for the restart window.

    Both halves are checked against the source, not assumed: if the handler is
    ever removed, requiring ExecReload would reintroduce the original footgun.
    That is why the signal.Notify grep below is a hard failure rather than a
    comment.
    """
    path = os.path.join(REPO, "system", "vmware-exporter.service")
    if not os.path.exists(path):
        return
    text = open(path, encoding="utf-8").read()

    live = [
        ln.strip() for ln in text.splitlines()
        if ln.strip() and not ln.strip().startswith("#")
    ]

    # The unit may only advertise reload while the binary can actually handle it.
    # Checking the source keeps the two from drifting apart in either direction.
    main_src = os.path.join(REPO, "vmware-exporter.go")
    handler_present = False
    if os.path.exists(main_src):
        src = open(main_src, encoding="utf-8").read()
        handler_present = "signal.Notify" in src and "syscall.SIGHUP" in src

    reload_lines = [ln for ln in live if ln.startswith("ExecReload=")]

    if not handler_present:
        # No handler: SIGHUP kills the process. ExecReload must not exist.
        for ln in reload_lines:
            failures.append(
                f"system/vmware-exporter.service: {ln!r}\n"
                "    vmware-exporter.go installs no SIGHUP handler (no "
                "signal.Notify + syscall.SIGHUP), so SIGHUP terminates the "
                "process. This directive would make `systemctl reload` stop the "
                "service while reporting success."
            )
    elif not reload_lines:
        failures.append(
            "system/vmware-exporter.service: no ExecReload\n"
            "    vmware-exporter.go handles SIGHUP and reloads -file plus the "
            "environment in place, so reload works. Without this directive "
            "`systemctl reload` fails and operators must restart, dropping "
            "metrics for the restart window.\n"
            "    Expected: ExecReload=/bin/kill -HUP $MAINPID"
        )
    else:
        # The handler only listens for SIGHUP. Any other signal here either does
        # nothing or kills the service, and the unit gives no hint which.
        for ln in reload_lines:
            if "-HUP" not in ln and "SIGHUP" not in ln:
                failures.append(
                    f"system/vmware-exporter.service: {ln!r}\n"
                    "    The exporter only handles SIGHUP. Any other signal is "
                    "either ignored or fatal.\n"
                    "    Expected: ExecReload=/bin/kill -HUP $MAINPID"
                )

    exec_start = [ln for ln in live if ln.startswith("ExecStart=")]
    if not exec_start:
        failures.append("system/vmware-exporter.service: no ExecStart")
        return

    for ln in exec_start:
        low = ln.lower()
        # The credential leak this unit was changed to avoid. `$ARGS` counts:
        # its expansion is what used to carry the password.
        if "password" in low or "username" in low or "$args" in low:
            failures.append(
                f"system/vmware-exporter.service: credentials reachable from "
                f"ExecStart: {ln!r}\n"
                "    Anything here lands in /proc/<pid>/cmdline. Pass "
                "credentials through EnvironmentFile with -envflag.enable."
            )
        if "-envflag.enable" not in ln:
            failures.append(
                "system/vmware-exporter.service: ExecStart lacks "
                "-envflag.enable, so the EnvironmentFile entries would be "
                "ignored entirely"
            )

        # The binary path must match what the READMEs tell people to install,
        # or the service dies with status=203/EXEC and the docs still look right.
        m = re.match(r"^ExecStart=(\S+)", ln)
        if m and m.group(1) != "/usr/bin/vmware-exporter":
            failures.append(
                f"system/vmware-exporter.service: ExecStart runs {m.group(1)!r}, "
                "but the READMEs install to /usr/bin/vmware-exporter.\n"
                "    A mismatch fails at start with status=203/EXEC."
            )

    # Flags named on ExecStart must exist, same reasoning as everywhere else.
    for ln in exec_start:
        for token in ln.split()[1:]:
            if not token.startswith("-"):
                continue
            name = token.lstrip("-").split("=", 1)[0]
            if name not in flags:
                failures.append(
                    f"system/vmware-exporter.service: flag {name!r} is not "
                    "registered by the exporter"
                )



def check_dashboards(failures: list[str]) -> None:
    """Assert the bundled dashboards match the metrics the exporter emits.

    The dashboards were migrated to the renamed metrics by
    scripts/migrate_dashboards.py, which also cancelled the unit conversions the
    exporter now applies itself. Both are invisible failures: a panel querying a
    metric that no longer exists renders an empty graph, and one that keeps a
    stale * 1024 renders a number that is wrong by three orders of magnitude. No
    error appears in either case, so nothing but a check like this catches a
    regression from a hand-edit.

    The checks live in migrate_dashboards.py rather than being duplicated here --
    a second copy of the rules would only drift from the first.
    """
    script = pathlib.Path(REPO) / "scripts" / "migrate_dashboards.py"
    dashboards = pathlib.Path(REPO) / "dashboards"
    if not script.exists() or not dashboards.is_dir():
        return

    spec = importlib.util.spec_from_file_location("migrate_dashboards", script)
    if spec is None or spec.loader is None:
        failures.append(f"could not load {script.name} to check the dashboards")
        return

    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
        by_old = module.load_map()
    except Exception as exc:  # noqa: BLE001 - report, do not crash the whole check
        failures.append(f"could not load {script.name} to check the dashboards: {exc}")
        return

    for path in sorted(dashboards.glob("*.json")):
        try:
            problems = module.check(path, by_old)
        except Exception as exc:  # noqa: BLE001
            failures.append(f"{path.name}: could not be checked: {exc}")
            continue
        for problem in problems:
            failures.append(f"{path.name}: {problem}")


def main() -> int:
    flags, source = registered_flags()
    if not flags:
        print("error: found no flag registrations; is this the repo root?", file=sys.stderr)
        return 2

    if source == "source":
        # Say so rather than degrading quietly. In this mode the collector flags
        # are invisible, so a valid VMWARE_collector_* variable would be
        # reported as unmatched -- the reader needs to know that before acting
        # on such a failure.
        print(
            "warning: could not build the exporter, so flag names were grepped "
            "from the source.\n"
            "  Constructed names like collector.vm are invisible that way and "
            "may be reported as unmatched.\n"
            "  Install Go to get the authoritative list from `--help`.",
            file=sys.stderr,
        )

    failures: list[str] = []
    files = scanned_files()
    check_leaks(failures, files)
    check_compose(failures, flags)
    check_conf(failures, flags)
    check_unit(failures, flags)
    check_readme_flags(failures, flags, source)
    check_dependabot(failures)
    check_dashboards(failures)

    if failures:
        print("config check FAILED:\n")
        for f in failures:
            print(f"  - {f}")
        return 1

    print(
        f"config check OK ({len(flags)} flags known via {source}, "
        f"{len(files)} files scanned)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
