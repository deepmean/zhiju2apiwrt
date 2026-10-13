"""Report (and optionally strip) UTF-8 BOMs in the repo.

A BOM breaks the first line of a shell script: the kernel sees `#!/bin/sh` and the
shebang is not recognised, which on the router means `sh script.sh` complains and
`./script.sh` may not run at all. Go tolerates a BOM, but nothing else here does.
"""
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SUFFIXES = {".go", ".sh", ".yml", ".yaml", ".md", ".py", ".example", ".map", ".txt", ""}
SKIP_DIRS = {".git", "dist", "recon", "node_modules"}

strip = "--fix" in sys.argv

found = []
for p in ROOT.rglob("*"):
    if not p.is_file():
        continue
    if any(part in SKIP_DIRS for part in p.relative_to(ROOT).parts):
        continue
    if p.suffix not in SUFFIXES and p.name not in {"init.d-zhiju2api", ".gitignore", ".gitattributes"}:
        continue
    raw = p.read_bytes()
    if raw.startswith(b"\xef\xbb\xbf"):
        found.append(p.relative_to(ROOT))
        if strip:
            p.write_bytes(raw[3:])

print(f"files with a BOM: {len(found)}")
for f in found:
    print("  ", f)
if found and not strip:
    print("\nre-run with --fix to strip them")
elif found:
    print("\nstripped")
