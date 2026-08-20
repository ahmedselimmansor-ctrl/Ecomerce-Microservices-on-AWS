#!/usr/bin/env python3
"""
Asserts that every internal link in the frontends points at a route that exists.

A dead internal link is invisible to every other check here: it typechecks, it
builds, and it renders as a working anchor that 404s when clicked. The footer
alone links to six pages, and pages get renamed.

Route resolution follows the App Router's rules: `(groups)` consume no URL
segment, `[dynamic]` matches anything, and a directory is only a route if it
holds a page.tsx.

Two link forms are recognised: the JSX attribute `href="/x"` and the object
literal `href: '/x'` that every nav array in this codebase uses. Missing the
second was the first bug in this script — it reported "0 dead" while checking
barely a third of the links.

A template literal is a runtime value and this cannot know what it evaluates to.
Stated rather than implied.

`dynamicParams = false` is honoured. A `[slug]` directory otherwise matches
anything, so `/legal/privacy-policy` "resolved" against `legal/[slug]` even
though the route declares a fixed set and 404s on the rest. That was the second
bug, and both were found by deliberately breaking a link and noticing nothing
failed.

Usage:  scripts/link-check.py
Exit:   0 clean, 1 dead links.
"""

from __future__ import annotations

import os
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
APPS = [("storefront", REPO / "apps/storefront"), ("admin", REPO / "apps/admin")]


def fixed_slugs(page: Path, app_dir: Path) -> set[str] | None:
    """
    The slugs a `dynamicParams = false` route actually serves, or None.

    None means the segment genuinely matches anything — a product slug, an order
    id — and the link cannot be checked further.

    When the route pins its params, the values come from whichever module it
    imports them from. Scanning that module for `slug: '...'` is narrow, and it
    is enough for the only routes in this repository that do it.
    """
    source = page.read_text()

    if not re.search(r"dynamicParams\s*=\s*false", source):
        return None

    slugs: set[str] = set()

    for module in re.findall(r"from\s+'@/([\w/.-]+)'", source):
        candidate = app_dir / "src" / f"{module}.ts"
        if not candidate.exists():
            candidate = app_dir / "src" / f"{module}.tsx"
        if candidate.exists():
            slugs.update(re.findall(r"slug:\s*'([\w-]+)'", candidate.read_text()))

    # Literals in the page itself, for a route that lists them inline.
    slugs.update(re.findall(r"slug:\s*'([\w-]+)'", source))

    return slugs or None


def route_exists(app_dir: Path, path: str) -> bool:
    segments = [s for s in path.strip("/").split("/") if s]

    def walk(directory: Path, remaining: list[str]) -> bool:
        if not remaining:
            return (directory / "page.tsx").exists()
        if not directory.is_dir():
            return False

        head, tail = remaining[0], remaining[1:]

        for entry in directory.iterdir():
            if not entry.is_dir():
                continue
            name = entry.name

            if name.startswith("(") and name.endswith(")"):
                # A route group is organisational; it consumes no URL segment.
                if walk(entry, remaining):
                    return True

            elif name == head:
                if walk(entry, tail):
                    return True

            elif name.startswith("[") and name.endswith("]"):
                page = entry / "page.tsx"
                if page.exists():
                    pinned = fixed_slugs(page, app_dir)
                    # A pinned route serves exactly these and 404s on the rest.
                    if pinned is not None and head not in pinned:
                        continue
                if walk(entry, tail):
                    return True

        return False

    return walk(app_dir / "src/app", segments)


def main() -> int:
    findings: list[str] = []
    total = 0

    for name, app in APPS:
        if not app.exists():
            continue

        hrefs: dict[str, str] = {}
        for path in (app / "src").rglob("*.ts*"):
            if any(part in {"node_modules", ".next"} for part in path.parts):
                continue
            text = path.read_text()
            # `href="/x"` and `href: '/x'` — the second is what every nav array
            # in this codebase uses, and missing it was this script's own first
            # bug.
            for pattern in (r'href="(/[^"{}#?]*)"', r"href:\s*'(/[^'{}#?]*)'"):
                for m in re.finditer(pattern, text):
                    hrefs.setdefault(m.group(1), str(path.relative_to(REPO)))

        total += len(hrefs)
        for href, source in sorted(hrefs.items()):
            # An API route is served by route.ts, not page.tsx.
            if href.startswith("/api/"):
                continue
            if not route_exists(app, href):
                findings.append(f"{source}: href=\"{href}\" has no page")

    for finding in findings:
        print(f"  {finding}")

    print(f"\n  {total} internal links, {len(findings)} dead")
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
