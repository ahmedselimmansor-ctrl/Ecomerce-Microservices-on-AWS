#!/usr/bin/env python3
"""
Asserts that every image can actually be built, without building it.

Three places name an image and they have to agree: the Dockerfile says what it
copies, docker-compose says what context to hand it, and the CI matrix says the
same thing again. When they disagree the failure is a build error in a job that
only runs on `main` — which is how a Dockerfile referencing
`../../libs/ts-contracts` survived in this repository: that is not a path Docker
will follow, it is a hard error, and nothing local ever tried.

What it checks:

  1. every Dockerfile in the tree appears in the CI matrix, and vice versa
  2. every COPY source resolves to something that exists under the root
  3. no COPY escapes the build context with `..`
  4. compose and CI both use the repository root as the context
  5. every image referenced by the Kubernetes manifests has a Dockerfile

It does not run `docker build`. That takes twenty minutes for thirteen
multi-arch images and belongs in CI; this is the check that fails a pull request
in under a second.

Usage:  scripts/image-check.py
Exit:   0 clean, 1 findings.
"""

from __future__ import annotations

import os
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

SKIP_DIRS = {"node_modules", ".git", ".next", "dist", "target", "build"}


def find_dockerfiles() -> list[Path]:
    out: list[Path] = []
    for root, dirs, files in os.walk(REPO):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        if "Dockerfile" in files:
            out.append(Path(root) / "Dockerfile")
    return sorted(out)


def copy_sources(text: str) -> list[str]:
    """
    Every COPY source, excluding stage references.

    `COPY --from=build /app/dist ./dist` copies from an earlier stage, not from
    the context, so its source is not a path that has to exist on disk.
    """
    sources: list[str] = []

    for line in text.splitlines():
        stripped = line.strip()
        if not stripped.upper().startswith("COPY "):
            continue
        if "--from=" in stripped:
            continue

        parts = [p for p in stripped.split()[1:] if not p.startswith("--")]
        # The last token is the destination.
        sources.extend(parts[:-1])

    return sources


def main() -> int:
    findings: list[str] = []
    dockerfiles = find_dockerfiles()

    # ---------------------------------------------------- COPY sources exist
    for path in dockerfiles:
        rel = path.relative_to(REPO)
        text = path.read_text()

        for source in copy_sources(text):
            if source.startswith("/"):
                continue

            if ".." in Path(source).parts:
                findings.append(
                    f"{rel}: `COPY {source}` escapes the build context — Docker "
                    f"rejects this outright, it is not a path it will follow"
                )
                continue

            # Globs are resolved by Docker, not here.
            if any(ch in source for ch in "*?["):
                stem = source.split("*")[0].rstrip("/")
                target = REPO / stem if stem else REPO
                if stem and not target.parent.exists():
                    findings.append(f"{rel}: `COPY {source}` matches nothing under the root")
                continue

            if not (REPO / source).exists():
                findings.append(f"{rel}: `COPY {source}` does not exist relative to the root")

    # ------------------------------------------------------------ CI matrix
    ci_path = REPO / ".github/workflows/ci.yml"
    ci_dockerfiles: set[str] = set()

    if ci_path.exists():
        ci = ci_path.read_text()
        ci_dockerfiles = set(re.findall(r"dockerfile:\s*(\S+/Dockerfile)", ci))

        listed = {str(p.relative_to(REPO)) for p in dockerfiles}

        for missing in sorted(listed - ci_dockerfiles):
            findings.append(f"{missing}: not in the CI images matrix — nothing ever builds it")
        for extra in sorted(ci_dockerfiles - listed):
            findings.append(f"CI matrix names {extra}, which does not exist")

        # The context must be the root, because ten of the Dockerfiles copy
        # from libs/ or contracts/.
        for context in set(re.findall(r"^\s+context:\s*(\S+)\s*$", ci, re.M)):
            if context not in {".", "./"}:
                findings.append(
                    f"CI builds with context '{context}'; the Dockerfiles expect the root"
                )

    # -------------------------------------------------------------- compose
    compose_path = REPO / "docker-compose.yml"
    if compose_path.exists():
        compose = compose_path.read_text()

        for legacy in re.findall(r"^\s+build:\s*\./(\S+)\s*$", compose, re.M):
            findings.append(
                f"docker-compose builds ./{legacy} with a directory context; "
                f"the Dockerfiles expect the root"
            )

        for context in set(re.findall(r"^\s+context:\s*(\S+)\s*$", compose, re.M)):
            if context not in {".", "./"}:
                findings.append(
                    f"docker-compose builds with context '{context}'; "
                    f"the Dockerfiles expect the root"
                )

        for dockerfile in set(re.findall(r"^\s+dockerfile:\s*(\S+)\s*$", compose, re.M)):
            if not (REPO / dockerfile).exists():
                findings.append(f"docker-compose names {dockerfile}, which does not exist")

    # ----------------------------------------------- Kubernetes image names
    image_names = {p.parent.name for p in dockerfiles}
    for manifest in sorted((REPO / "infra/k8s/base").rglob("*.yaml")):
        for image in re.findall(r"^\s+image:\s*\S+/souq/([\w-]+):", manifest.read_text(), re.M):
            if image not in image_names:
                findings.append(
                    f"{manifest.relative_to(REPO)}: deploys image '{image}', "
                    f"which has no Dockerfile"
                )

    for finding in findings:
        print(f"  {finding}")

    print(
        f"\n  {len(dockerfiles)} Dockerfiles, {len(ci_dockerfiles)} in the CI matrix, "
        f"{len(findings)} finding(s)"
    )
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
