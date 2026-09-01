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

2. **Credentials on the command line.** Anything in a compose `command:` ends
   up in the container's cmdline, readable via `docker inspect`, via `ps`
   inside the container, and via /proc. Moving the password to `environment:`
   plus `-envflag.enable` keeps it out.

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

Exit code is 0 when clean, 1 when any check fails, 2 on a missing dependency.
"""

from __future__ import annotations

import os
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
    """vmware.conf holds a single ARGS= line of flags; validate the flag names."""
    path = os.path.join(REPO, "vmware.conf")
    if not os.path.exists(path):
        return
    text = open(path, encoding="utf-8").read()

    args_lines = [
        ln for ln in text.splitlines()
        if ln.strip().startswith("ARGS=") and not ln.strip().startswith("#")
    ]
    if not args_lines:
        failures.append("vmware.conf: no uncommented ARGS= line found")
        return

    for ln in args_lines:
        body = ln.split("=", 1)[1].strip().strip('"')
        for token in body.split():
            if not token.startswith("-"):
                continue
            name = token.lstrip("-").split("=", 1)[0]
            if name not in flags:
                failures.append(
                    f"vmware.conf: flag {name!r} is not registered by the exporter"
                )
        # A placeholder must stay a placeholder.
        for token in body.split():
            if token.startswith("-vmware.password="):
                value = token.split("=", 1)[1]
                if not (value.startswith("<") and value.endswith(">")):
                    failures.append(
                        f"vmware.conf: -vmware.password carries a literal value "
                        f"({value!r}); it must stay a <PLACEHOLDER>"
                    )


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
    check_readme_flags(failures, flags, source)
    check_dependabot(failures)

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
