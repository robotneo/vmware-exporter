#!/usr/bin/env python3
"""Guard against credentials and unreachable env vars in shipped config.

Run this in CI:

    python3 scripts/check_config.py

Three classes of problem are caught, in increasing order of subtlety:

1. **Credential regressions.** Real passwords and internal addresses used to
   live in docker-compose.yml, vmware.conf and README-zh.md. They are in the
   git history and cannot be removed from it (the repo is a fork, so rewriting
   history would break the relationship), which makes it all the more important
   that no new one gets added.

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

Exit code is 0 when clean, 1 when any check fails, 2 on a missing dependency.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys

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
SCANNED_FILES = [
    "docker-compose.yml",
    "vmware.conf",
    "README.md",
    "README-zh.md",
    "system/vmware-exporter.service",
]


def registered_flags() -> set[str]:
    """Every flag name the exporter registers, read from the source."""
    out = subprocess.run(
        ["git", "grep", "-hoE", r'flag\.[A-Za-z]+\(\s*"[^"]+"', "--", "*.go"],
        cwd=REPO,
        capture_output=True,
        text=True,
        check=False,
    )
    return set(re.findall(r'"([^"]+)"', out.stdout))


def check_leaks(failures: list[str]) -> None:
    for rel in SCANNED_FILES:
        path = os.path.join(REPO, rel)
        if not os.path.exists(path):
            continue
        text = open(path, encoding="utf-8").read()
        for secret in KNOWN_LEAKS:
            if secret in text:
                failures.append(
                    f"{rel}: previously leaked value {secret!r} is present again"
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
    flags = registered_flags()
    if not flags:
        print("error: found no flag registrations; is this the repo root?", file=sys.stderr)
        return 2

    failures: list[str] = []
    check_leaks(failures)
    check_compose(failures, flags)
    check_conf(failures, flags)

    if failures:
        print("config check FAILED:\n")
        for f in failures:
            print(f"  - {f}")
        return 1

    print(f"config check OK ({len(flags)} flags known, {len(SCANNED_FILES)} files scanned)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
