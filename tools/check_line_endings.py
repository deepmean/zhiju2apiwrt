"""Line-ending and integrity check for the tracked text files.

A mismatch between the reported line count and the line numbers inside the file means the
file mixes LF and CRLF, or contains lone CRs — worth knowing before it bites a shell script
on the router.
"""
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
SKIP = {".git", "node_modules", "tools/verify/out"}

mixed = []
for p in sorted(ROOT.rglob("*")):
    if not p.is_file():
        continue
    rel = p.relative_to(ROOT).as_posix()
    if any(rel.startswith(s) or f"/{s}/" in f"/{rel}" for s in SKIP):
        continue
    if p.suffix not in {".go", ".md", ".sh", ".py", ".mjs", ".yml", ".yaml", ".txt", ".map", ".example", ""}:
        continue
    raw = p.read_bytes()
    crlf = raw.count(b"\r\n")
    lone_cr = raw.count(b"\r") - crlf
    lf = raw.count(b"\n") - crlf
    if crlf and lf:
        mixed.append((rel, crlf, lf, lone_cr))
    elif lone_cr:
        mixed.append((rel, crlf, lf, lone_cr))

if not mixed:
    print("所有文件行尾一致（无混用）")
else:
    print(f"{len(mixed)} 个文件行尾混用或含孤立 CR：")
    for rel, crlf, lf, lone in mixed:
        print(f"  {rel:40} CRLF={crlf} LF={lf} 孤立CR={lone}")

# Shell scripts must not carry CRLF: the kernel would not recognise the shebang.
for p in sorted((ROOT / "tools").rglob("*.sh")):
    raw = p.read_bytes()
    if b"\r\n" in raw:
        print(f"  !! {p.relative_to(ROOT)} 含 CRLF —— shebang 会失效")
