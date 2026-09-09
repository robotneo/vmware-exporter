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

8. **Metrics missing from the metric references, or documented but gone.** Same
   bidirectional argument as the README flag check, and the same failure mode:
   a metric added without a doc entry is undiscoverable, and a doc entry left
   behind after a rename points users at a series that will never appear. Both
   directions are silent in Prometheus -- an absent metric is indistinguishable
   from a target that has not scraped yet.

   Both docs/METRICS.md and its Chinese translation docs/METRICS-zh.md are
   checked. Holding only the English one to the contract would let a rename be
   enforced there and skipped in the translation, which is the drift both files
   promise in their own header to prevent.

   The metric list is parsed from the Go sources rather than from a running
   binary, because most metrics only materialise once vCenter has been queried:
   `/metrics` on a process that never logged in emits the self-monitoring
   handful and nothing else. Four declaration shapes are covered, and the
   parser fails loudly if it can no longer resolve one of them, rather than
   quietly checking a shrinking subset.

9. **The scripted systemd bundle drifting from the binary or itself.**
   packaging/systemd/ is a second deployment story (a config.yaml loaded with
   -file, a unit, and install.sh, assembled by scripts/build-systemd-pkg.sh)
   that used to be validated by nothing. The config's keys are checked against
   the registered flags (including commented example lines an operator will
   uncomment), the mapping must stay flat as config.go requires, the unit's
   ExecStart binary and -file path are checked against install.sh's actual
   install destinations, ExecReload is held to the same SIGHUP-source contract
   as the other unit, and every file the build script tars up must exist.

Exit code is 0 when clean, 1 when any check fails, 2 on a missing dependency.
"""

from __future__ import annotations

import glob
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

# The metric prefix, as set in vmware-exporter.go.
NAMESPACE = "vmware"

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


# ── packaging/systemd (the scripted one-shot deployment bundle) ──────────────
#
# This bundle is a second, parallel deployment story: config.yaml + a unit that
# loads it with -file + install.sh/uninstall.sh, assembled by
# scripts/build-systemd-pkg.sh. Unlike vmware.conf + system/vmware-exporter.service,
# none of it used to be validated here -- so the shipped config could name a flag
# the binary no longer registers and every build stayed green until an operator
# hit "config sets unknown flag" at install time. The checks below hold the four
# files to one another and to the binary:
#
#   config.yaml key  -> a registered flag, and a flat mapping as config.go demands
#   unit ExecStart   -> /usr/bin/vmware-exporter -file=<install.sh CONF_DST>
#   install.sh paths -> the binary and config paths the unit actually references
#   build script     -> every file it copies into the tarball exists
PKG_DIR_REL = os.path.join("packaging", "systemd")
PKG_CONF_REL = os.path.join(PKG_DIR_REL, "config.yaml")
PKG_UNIT_REL = os.path.join(PKG_DIR_REL, "vmware-exporter.service")
PKG_INSTALL_REL = os.path.join(PKG_DIR_REL, "install.sh")
PKG_BUILD_REL = os.path.join("scripts", "build-systemd-pkg.sh")


def _pkg_conf_keys(text: str) -> list[str]:
    """Config keys in packaging/systemd/config.yaml, active and commented.

    Commented example lines (e.g. ``# web.config.file: ...``) are included: they
    are the first thing an operator uncomments, so an unknown flag there is just
    as broken. Prose containing a colon must NOT be read as a key. YAML requires
    whitespace (or end-of-line) after the ``key:`` colon, which prose does not
    have -- that single rule separates ``web.config.file: /x`` (real) from
    ``root:root 0600`` (prose) and ``https://...`` (a URL).
    """
    keys = []
    key_re = re.compile(r"^[#\s]*([a-z0-9][a-z0-9._-]*):(?:\s+.*)?\s*$")
    for line in text.splitlines():
        m = key_re.match(line)
        if m:
            keys.append(m.group(1))
    return keys


def check_packaging(failures: list[str], flags: set[str]) -> None:
    """Hold the scripted systemd deployment bundle together and against the binary."""
    pkg_dir = os.path.join(REPO, PKG_DIR_REL)
    if not os.path.isdir(pkg_dir):
        # The bundle is optional in a checkout; only validate it when present.
        return

    # ── 1. config.yaml: flat mapping, every key a real flag ──────────────────
    conf_path = os.path.join(REPO, PKG_CONF_REL)
    if os.path.exists(conf_path):
        raw = open(conf_path, encoding="utf-8").read()

        # The structural rule config.go enforces: one flat level of scalar keys.
        # A nested mapping/sequence loads fine in YAML but the loader rejects it,
        # so fail here where the offending line is in front of the author.
        parsed = yaml.safe_load(raw)
        if not isinstance(parsed, dict):
            failures.append(
                f"{PKG_CONF_REL}: top level must be a flat mapping of "
                "flag: value, not a nested structure (config.go cannot parse it)"
            )
        else:
            for key, value in parsed.items():
                if isinstance(value, (dict, list)):
                    failures.append(
                        f"{PKG_CONF_REL}: key {key!r} maps to a nested value; "
                        "the config format is flat -- write flag-name: value with "
                        "no indentation"
                    )

        for key in _pkg_conf_keys(raw):
            if key not in flags:
                failures.append(
                    f"{PKG_CONF_REL}: key {key!r} is not a registered flag; "
                    "the loader aborts with 'config sets unknown flag'. If the "
                    "flag was renamed or removed, update this shipped template."
                )

    # ── 2. the unit: ExecStart binary + -file, ExecReload only if handled ────
    unit_path = os.path.join(REPO, PKG_UNIT_REL)
    unit_conf_path = None
    if os.path.exists(unit_path):
        text = open(unit_path, encoding="utf-8").read()
        live = [
            ln.strip() for ln in text.splitlines()
            if ln.strip() and not ln.strip().startswith("#")
        ]

        exec_start = [ln for ln in live if ln.startswith("ExecStart=")]
        if not exec_start:
            failures.append(f"{PKG_UNIT_REL}: no ExecStart")
        for ln in exec_start:
            low = ln.lower()
            if "password" in low or "username" in low:
                failures.append(
                    f"{PKG_UNIT_REL}: credentials on ExecStart: {ln!r}\n"
                    "    Anything here lands in /proc/<pid>/cmdline. This bundle "
                    "passes credentials through -file (config.yaml), not the cmdline."
                )
            # ln is "ExecStart=/usr/bin/vmware-exporter -file=..."; argv[0] is the
            # binary, argv[1:] are the flags.
            argv = ln.split()
            tokens = argv[1:]
            if not tokens or not tokens[0].startswith("-file="):
                failures.append(
                    f"{PKG_UNIT_REL}: ExecStart must load the config with "
                    f"-file=</path/config.yaml>, got: {ln!r}"
                )
            else:
                unit_conf_path = tokens[0].split("=", 1)[1]

            # Validate every flag named on the line against the binary.
            for token in tokens:
                if not token.startswith("-"):
                    continue
                name = token.lstrip("-").split("=", 1)[0]
                if name not in flags:
                    failures.append(
                        f"{PKG_UNIT_REL}: flag {name!r} is not registered by the "
                        "exporter"
                    )

        # Same bidirectional reload contract as system/vmware-exporter.service:
        # advertise ExecReload only while the binary actually handles SIGHUP.
        main_src = os.path.join(REPO, "vmware-exporter.go")
        handler_present = False
        if os.path.exists(main_src):
            src = open(main_src, encoding="utf-8").read()
            handler_present = "signal.Notify" in src and "syscall.SIGHUP" in src
        reload_lines = [ln for ln in live if ln.startswith("ExecReload=")]
        if handler_present and not reload_lines:
            failures.append(
                f"{PKG_UNIT_REL}: no ExecReload, but vmware-exporter.go handles "
                "SIGHUP; without it operators restart and drop metrics. "
                "Expected: ExecReload=/bin/kill -HUP $MAINPID"
            )
        for ln in reload_lines:
            if "-HUP" not in ln and "SIGHUP" not in ln:
                failures.append(
                    f"{PKG_UNIT_REL}: {ln!r} -- the exporter only handles SIGHUP"
                )

    # ── 3. install.sh paths must match the unit's binary and -file target ────
    install_path = os.path.join(REPO, PKG_INSTALL_REL)
    if os.path.exists(install_path) and unit_conf_path is not None:
        inst = open(install_path, encoding="utf-8").read()

        def sh_var(name: str) -> str | None:
            m = re.search(rf'^{name}=(?:"([^"]*)"|\'([^\']*)\'|(\S+))',
                          inst, re.MULTILINE)
            if not m:
                return None
            return next(g for g in m.groups() if g is not None)

        bin_name = sh_var("BIN_NAME")
        conf_dir = sh_var("CONF_DIR")
        conf_name = sh_var("CONF_DST")
        if bin_name and conf_dir:
            # Strip the ${DESTDIR} staging prefix: install paths are absolute on
            # the target host, and the unit references the same absolute paths.
            real_conf_dir = conf_dir.replace("${DESTDIR}", "")
            expected_conf = f"{real_conf_dir}/config.yaml"
            if unit_conf_path != expected_conf:
                failures.append(
                    f"{PKG_UNIT_REL}: ExecStart loads {unit_conf_path!r}, but "
                    f"{PKG_INSTALL_REL} installs the config to {expected_conf!r}. "
                    "The service would start against a config that is not there."
                )
            expected_bin = f"/usr/bin/{bin_name}"
            if exec_start:
                actual_bin = exec_start[0].split()[0].split("=", 1)[1]
                if actual_bin != expected_bin:
                    failures.append(
                        f"{PKG_UNIT_REL}: ExecStart runs {actual_bin!r}, but "
                        f"{PKG_INSTALL_REL} installs the binary as "
                        f"{expected_bin!r} (BIN_NAME={bin_name!r})."
                    )
        elif conf_name is None:
            failures.append(
                f"{PKG_INSTALL_REL}: could not locate CONF_DIR/CONF_DST; the "
                "deployed config path can no longer be checked against the unit"
            )

    # ── 4. the build script must copy only files that actually ship ──────────
    build_path = os.path.join(REPO, PKG_BUILD_REL)
    if os.path.exists(build_path):
        build = open(build_path, encoding="utf-8").read()
        for m in re.finditer(r'\$\{PKG_DIR\}/([A-Za-z0-9._-]+)', build):
            shipped = os.path.join(pkg_dir, m.group(1))
            if not os.path.exists(shipped):
                failures.append(
                    f"{PKG_BUILD_REL}: copies {PKG_DIR_REL}/{m.group(1)} into the "
                    "tarball, but that file does not exist -- the build would fail"
                )


DOC_REL = os.path.join("docs", "METRICS.md")
DOC_REL_ZH = os.path.join("docs", "METRICS-zh.md")

# Every metrics reference that must stay in step with the code. The Chinese
# translation is checked too, not just the English original: enforcing only one
# of them would let a rename be caught in METRICS.md and skipped in
# METRICS-zh.md, leaving a translation that documents series the exporter no
# longer emits. Both files open by declaring themselves part of the contract, so
# both have to be held to it.
DOC_RELS = (DOC_REL, DOC_REL_ZH)

# Metrics the parser must not expect to find in the references by name, with the
# reason. Everything else is required to match in both directions.
#
# Performance counters are the interesting case: their names are computed at
# scrape time from vCenter's own counter metadata (see perfnames.go), so there
# is no literal `vmware_host_cpu_usage_hertz` anywhere in the source to compare
# against. The references document them by naming rule instead, and the rule
# itself is covered by TestPerfCounterNamesAreMapped in the Go tests.
DOC_ONLY_PREFIXES = (
    # Emitted by prometheus/common versioncollector, not by this repository.
    "vmware_exporter_build_info",
)

# The heading that introduces the illustrative performance-counter rows, per
# document. Rows under it are worked examples of the naming rule rather than a
# registry of emitted series, so they are excluded from the comparison. The
# translated heading differs, hence the per-file mapping -- and a heading that
# stops matching is reported as a failure rather than silently excluding
# nothing.
PERF_DOC_SECTIONS = {
    DOC_REL: "## Performance counters",
    DOC_REL_ZH: "## 性能计数器",
}


def go_metric_sources() -> dict[str, str]:
    """Every non-test Go file that can declare a metric name."""
    out: dict[str, str] = {}
    pattern = os.path.join(REPO, "vmware", "collectors", "*.go")
    for path in sorted(glob.glob(pattern)):
        if path.endswith("_test.go"):
            continue
        out[path] = open(path, encoding="utf-8").read()
    for extra in ("vmware-exporter.go", os.path.join("internal", "collector", "set.go")):
        path = os.path.join(REPO, extra)
        if os.path.exists(path):
            out[path] = open(path, encoding="utf-8").read()
    return out


def balanced_args(text: str, start: int) -> str:
    """Content of a call whose opening paren ends at `start`.

    A plain regex cannot do this: `d("capacity", "..."+deprecatedFor(ns, sub,
    "capacity_bytes"), "dsmo", ...)` contains a nested call whose last argument
    is a metric name, and a non-greedy match to the first `)` truncates the
    argument list right through it.
    """
    depth, instr, esc = 1, False, False
    i = start
    while i < len(text) and depth > 0:
        ch = text[i]
        if instr:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                instr = False
        elif ch == '"':
            instr = True
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        i += 1
    return text[start:i - 1]


def subsystem_consts(allsrc: str) -> dict[str, str]:
    """`xxxSubsystem = "yyy"` constants, which live in the collector files."""
    return dict(re.findall(r'(\w+Subsystem)\s*=\s*"([^"]+)"', allsrc))


def pointer_subsystems(src: str) -> str | None:
    """Resolve `*subsystem` for the esxcli collectors.

    Those two collectors pass `&esxcliXxxSubsystem` down through several helpers
    and build the name from `*subsystem`, so the constant is not visible at the
    BuildFQName call site. The call sites are unambiguous -- one constant per
    file -- so take it from there.
    """
    found = set(re.findall(r"&(\w+Subsystem)\b", src))
    if len(found) == 1:
        return found.pop()
    return None


def metrics_from_source(failures: list[str]) -> set[str]:
    """Every metric name declared in the Go sources.

    Parsed rather than probed. A running exporter only exposes what the last
    scrape produced, and without a reachable vCenter that is the self-monitoring
    handful -- so `--help`-style introspection, which works for flags, cannot
    enumerate metrics.

    Four declaration shapes exist, and each is claimed to yield at least one
    name below. That assertion is the part that matters: if a refactor renames
    `buildXxxDescs` or stops using `BuildFQName`, the parser would otherwise
    return a smaller set and the bidirectional check would pass by comparing
    almost nothing against almost nothing.
    """
    srcs = go_metric_sources()
    allsrc = "".join(srcs.values())
    consts = subsystem_consts(allsrc)

    found: set[str] = set()
    counts = {"descs": 0, "inline": 0, "gauge": 0, "vsanperf": 0}

    for path, src in srcs.items():
        ptr_const = pointer_subsystems(src)

        # Shape 1: the `d("name", help, labels...)` helper inside each
        # buildXxxDescs, where the subsystem comes from the enclosing
        # BuildFQName(namespace, xxxSubsystem, name).
        for chunk in re.split(r"\nfunc (build\w+Descs)\(namespace string\)", src)[2::2]:
            body = chunk.split("\nfunc ")[0]
            m = re.search(r"BuildFQName\(namespace,\s*(\w+),", body)
            if not m:
                continue
            subsys = consts.get(m.group(1), m.group(1))
            for call in re.finditer(r'\bd\(\s*"([^"]+)"', body):
                found.add(f"{NAMESPACE}_{subsys}_{call.group(1)}")
                counts["descs"] += 1

        # Shape 2: BuildFQName called directly in a collector body.
        for m in re.finditer(r"BuildFQName\(", src):
            args = [a.strip() for a in balanced_args(src, m.end()).split(",")]
            if len(args) < 3:
                continue
            ns, sub, name = args[0], args[1], args[2]
            if "namespace" not in ns.lower():
                continue

            def literal(token: str) -> str | None:
                quoted = re.fullmatch(r'"([^"]*)"', token)
                if quoted:
                    return quoted.group(1)
                if token in consts:
                    return consts[token]
                # `*subsystem` in the esxcli helpers -- see pointer_subsystems.
                if token == "*subsystem" and ptr_const in consts:
                    return consts[ptr_const]
                return None

            sub_v, name_v = literal(sub), literal(name)
            if name_v is None:
                continue
            if sub_v is None:
                failures.append(
                    f"{os.path.relpath(path, REPO)}: BuildFQName subsystem "
                    f"{sub!r} could not be resolved to a string, so any metric "
                    "it declares is invisible to the METRICS.md check.\n"
                    "    Teach scripts/check_config.py how to resolve it "
                    "rather than leaving the metric unchecked."
                )
                continue
            parts = [NAMESPACE] + ([sub_v] if sub_v else []) + [name_v]
            found.add("_".join(parts))
            counts["inline"] += 1

        # Shape 3: package-level gauges in the root package (the reload metrics).
        for m in re.finditer(r'GaugeOpts\{[^}]*?Name:\s*"([^"]+)"', src, re.S):
            found.add(m.group(1))
            counts["gauge"] += 1

        # Shape 4: the vSAN performance whitelist. One metric per entry; the
        # entity type is a label value, not part of the name.
        wl = re.search(r"vsanPerfLabelWhitelist = \[\]string\{(.*?)\n\}", src, re.S)
        if wl:
            subsys = consts.get("vsanPerfSubsystem", "vsan_perf")
            for label in re.findall(r'"([^"]+)"', wl.group(1)):
                found.add(f"{NAMESPACE}_{subsys}_{label}")
                counts["vsanperf"] += 1

    for shape, n in sorted(counts.items()):
        if n == 0:
            failures.append(
                f"{DOC_REL} check: the {shape!r} metric declaration shape "
                "matched nothing.\n"
                "    Either it was refactored away (update this parser) or the "
                "parser broke. Silently checking fewer metrics is the one "
                "outcome this must not have."
            )

    return found
def metrics_from_doc(doc_rel: str, failures: list[str]) -> set[str]:
    """Every metric name in a table row of the given metrics document.

    Only table rows are counted. Prose mentions are ignored on purpose: a rename
    updates the table reliably but leaves the surrounding paragraph alone, so
    accepting prose would let a stale name count as documented forever. This is
    the same reasoning as check_readme_flags.
    """
    path = os.path.join(REPO, doc_rel)
    if not os.path.exists(path):
        failures.append(f"{doc_rel} does not exist, so every metric is undocumented")
        return set()
    text = open(path, encoding="utf-8").read()
    return set(re.findall(r"^\|\s*`(vmware_[a-z0-9_]+)`\s*\|", text, re.M))


def check_metrics(failures: list[str]) -> None:
    """Compare the metrics the code declares against every metrics document.

    Same shape as check_readme_flags, for the same reason. A metric with no doc
    entry is undiscoverable; a doc entry naming a metric the code no longer
    emits sends people to write queries against a series that will never
    appear. Neither direction produces an error anywhere else -- in Prometheus a
    metric that does not exist looks exactly like one whose target has not been
    scraped yet.

    Both the English and the Chinese reference are checked. Checking only the
    English one would leave the translation free to rot: a rename would be
    enforced in METRICS.md and silently skipped in METRICS-zh.md, which is
    exactly the drift both documents claim in their own header to prevent.
    Parsing is language-independent -- metric names and the table pipes are
    identical in both -- so the same parser serves both files.
    """
    code = metrics_from_source(failures)

    for doc_rel in DOC_RELS:
        check_one_metrics_doc(doc_rel, code, failures)


def check_one_metrics_doc(doc_rel: str, code: set[str], failures: list[str]) -> None:
    """Compare the code's metric set against one metrics document, both ways."""
    doc = metrics_from_doc(doc_rel, failures)

    # Names that legitimately appear on only one side. Both entries are
    # exhaustive: anything not listed here must match in both directions.
    #
    # Performance counters are computed at scrape time from vCenter's counter
    # metadata (perfnames.go), so no literal name exists in the source to
    # compare against. The documents describe them by naming rule instead, and
    # the rule is covered by the Go tests. Listing the handful that appear as
    # table rows keeps the rest of the check strict.
    doc_only = {
        # Emitted by prometheus/common's versioncollector, not by this repo.
        "vmware_exporter_build_info",
        # PerfMgr-derived, documented in the datastore section next to the
        # static ones.
        "vmware_datastore_disk_provisioned_bytes",
        "vmware_datastore_disk_used_bytes",
    }
    doc -= doc_only

    # Everything under the performance-counter heading is illustrative -- the
    # rows there are worked examples of the naming rule, not a registry of
    # emitted series.
    #
    # The slice must stop at the next `## ` heading. Taking everything after the
    # heading instead swallowed the deprecated-metrics and common-labels
    # sections, which do document real metrics -- and because they were then
    # subtracted from the doc side, 14 correctly documented metrics were
    # reported as missing.
    #
    # The heading is matched as a whole line. A plain substring split accepts any
    # heading that merely starts with the expected text, so renaming
    # `## Performance counters` to `## Performance counters (v2)` would still
    # match and the mismatch guard below would never fire.
    text = open(os.path.join(REPO, doc_rel), encoding="utf-8").read()
    heading = PERF_DOC_SECTIONS[doc_rel]
    perf_section = re.split(
        r"^" + re.escape(heading) + r"[ \t]*$", text, maxsplit=1, flags=re.M
    )
    if len(perf_section) == 2:
        body = re.split(r"^## ", perf_section[1], maxsplit=1, flags=re.M)[0]
        doc -= set(re.findall(r"`(vmware_[a-z0-9_]+)`", body))
    else:
        # The heading is how the illustrative rows get excluded. If it stops
        # matching -- renamed, translated, reformatted -- every worked example
        # under it turns into a phantom "documented but not declared" entry, and
        # the natural reaction is to add them to doc_only rather than to notice
        # the parser broke. Fail loudly instead.
        failures.append(
            f"{doc_rel}: heading {heading!r} not found, so the "
            "performance-counter examples cannot be excluded; update "
            "PERF_DOC_SECTIONS to match the document"
        )

    missing = sorted(code - doc)
    stale = sorted(doc - code)

    if missing:
        failures.append(
            f"{doc_rel}: {len(missing)} metric(s) declared in the code but not "
            "documented:\n" + "\n".join(f"    {n}" for n in missing)
        )
    if stale:
        failures.append(
            f"{doc_rel}: {len(stale)} metric(s) documented but not declared in "
            "the code:\n" + "\n".join(f"    {n}" for n in stale)
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
    check_packaging(failures, flags)
    check_readme_flags(failures, flags, source)
    check_dependabot(failures)
    check_dashboards(failures)
    check_metrics(failures)

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
