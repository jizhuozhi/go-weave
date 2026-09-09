#!/usr/bin/env python3
"""Documentation integrity checks.

1. Every relative Markdown link in the repository resolves to an existing file
   or directory (external URLs and bare anchors are ignored).
2. ``docs/en/`` and ``docs/zh/`` mirror each other file-for-file.

Run from anywhere; it resolves the repository root from its own location.
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

_LINK_RE = re.compile(r"\]\(([^)]+)\)")
_EXTERNAL = ("http://", "https://", "mailto:", "ftp://", "#")


def _markdown_files() -> list[Path]:
    return [p for p in sorted(ROOT.rglob("*.md")) if ".git" not in p.parts]


def _strip_code(text: str) -> str:
    """Remove fenced code blocks and inline code spans, so that Go code like
    ``New[T](nil)`` is never mistaken for a Markdown link."""
    lines = []
    in_fence = False
    for line in text.splitlines():
        if line.lstrip().startswith("```"):
            in_fence = not in_fence
            lines.append("")
            continue
        lines.append("" if in_fence else line)
    return re.sub(r"`[^`]*`", "", "\n".join(lines))


def check_links() -> int:
    """Return the number of broken relative links."""
    bad = 0
    for md in _markdown_files():
        text = _strip_code(md.read_text(encoding="utf-8"))
        for m in _LINK_RE.finditer(text):
            raw = m.group(1).strip()
            if raw.startswith(_EXTERNAL):
                continue
            # Drop a trailing anchor (#...) or query (?...).
            link = raw.split("#", 1)[0].split("?", 1)[0]
            if not link:
                continue
            target = (md.parent / link).resolve()
            if not target.exists():
                print(f"broken link in {md.relative_to(ROOT)}: {raw}")
                bad += 1
    return bad


def check_parity() -> int:
    """Return the number of EN/ZH file mismatches under docs/."""
    def rel(path: Path) -> set[str]:
        return {str(p.relative_to(path)) for p in path.rglob("*.md")}

    en = rel(ROOT / "docs" / "en")
    zh = rel(ROOT / "docs" / "zh")
    bad = 0
    for name in sorted(en - zh):
        print(f"docs/en/{name} has no docs/zh/{name} counterpart")
        bad += 1
    for name in sorted(zh - en):
        print(f"docs/zh/{name} has no docs/en/{name} counterpart")
        bad += 1
    return bad


def main() -> int:
    failures = check_links()
    failures += check_parity()
    if failures:
        print(f"documentation checks failed: {failures} problem(s)")
        return 1
    print("documentation checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
