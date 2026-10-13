#!/usr/bin/env python3
"""Keep the gateway's upstream session in sync with a browser login.

Why this exists
---------------
The 升智 web client authenticates with a single JWT in the `x-token-id` header. There is
no cookie, no refresh_token and no sliding renewal: the JWT carries a fixed `exp`
(about 100 days as of writing), and when it lapses the site asks for a phone number and
an SMS code again. So "auto refresh the login" cannot mean re-minting the token — it
means:

  * keep the gateway pointed at whatever token the browser currently holds,
  * and swap it in without restarting anything.

This script reads the token straight out of a Chromium profile (localStorage is stored
as UTF-16LE in the Local Storage leveldb, so the JWT is plain ASCII with NUL bytes
between characters) and pushes it to `POST /admin/tokens`. Run it once, or with
`--watch` on the machine where you are logged in.

Examples
--------
    # one-shot: read the default Chrome profile and print the token state
    python3 tools/zhiju_token_sync.py

    # push it to the router's gateway
    python3 tools/zhiju_token_sync.py --gateway http://192.168.26.1:8080 --key sk-... --push

    # keep it in sync every 10 minutes (e.g. from Task Scheduler / cron)
    python3 tools/zhiju_token_sync.py --gateway http://192.168.26.1:8080 --key sk-... --watch 600
"""
from __future__ import annotations

import argparse
import base64
import json
import re
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

JWT = re.compile(r"eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}")

DEFAULT_PROFILES = [
    # Windows
    r"%LOCALAPPDATA%\Google\Chrome Beta\User Data",
    r"%LOCALAPPDATA%\Google\Chrome\User Data",
    r"%LOCALAPPDATA%\Chromium\User Data",
    r"%LOCALAPPDATA%\Microsoft\Edge\User Data",
    r"%LOCALAPPDATA%\BraveSoftware\Brave-Browser\User Data",
    # Linux / macOS
    "~/.config/google-chrome",
    "~/.config/chromium",
    "~/Library/Application Support/Google/Chrome",
]


def expand(path: str) -> Path:
    return Path(path.replace("%LOCALAPPDATA%", str(Path.home() / "AppData" / "Local"))).expanduser()


def candidate_roots(extra: list[str]) -> list[Path]:
    roots: list[Path] = []
    for p in extra or DEFAULT_PROFILES:
        root = expand(p)
        if not root.exists():
            continue
        for child in sorted(root.iterdir()):
            if child.is_dir() and (child.name == "Default" or child.name.startswith("Profile")):
                ls = child / "Local Storage" / "leveldb"
                if ls.is_dir():
                    roots.append(ls)
    return roots


def decode_payload(token: str) -> dict:
    part = token.split(".")[1]
    part += "=" * (-len(part) % 4)
    try:
        return json.loads(base64.urlsafe_b64decode(part))
    except Exception:  # noqa: BLE001
        return {}


def find_token(roots: list[Path]) -> tuple[str, Path] | tuple[None, None]:
    best: tuple[str, Path] | tuple[None, None] = (None, None)
    for root in roots:
        for f in sorted(root.iterdir()):
            if f.suffix not in {".ldb", ".log"}:
                continue
            raw = f.read_bytes()
            for enc in ("utf-16-le", "utf-8", "latin-1"):
                for tok in JWT.findall(raw.decode(enc, errors="ignore")):
                    if best[0] is None or len(tok) > len(best[0]):
                        best = (tok, f)
    return best


def push(gateway: str, key: str, token: str, timeout: float = 30.0) -> dict:
    body = json.dumps({"tokens": [token]}).encode()
    req = urllib.request.Request(
        gateway.rstrip("/") + "/admin/tokens",
        data=body,
        method="POST",
        headers={"content-type": "application/json", "authorization": f"Bearer {key}"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode() or "{}")


def health(gateway: str, key: str, timeout: float = 15.0) -> dict:
    req = urllib.request.Request(
        gateway.rstrip("/") + "/health",
        headers={"authorization": f"Bearer {key}"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode() or "{}")


def main() -> int:
    ap = argparse.ArgumentParser(description="sync the browser session token into zhiju2api")
    ap.add_argument("--profile", action="append", default=[], help="Chromium user-data-dir (repeatable)")
    ap.add_argument("--gateway", default="", help="e.g. http://192.168.26.1:8080")
    ap.add_argument("--key", default="", help="the gateway API_KEY")
    ap.add_argument("--push", action="store_true", help="POST the token to /admin/tokens")
    ap.add_argument("--watch", type=int, default=0, metavar="SECONDS", help="loop every N seconds")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    def once() -> int:
        roots = candidate_roots(args.profile)
        if not roots:
            print("no Chromium profile with Local Storage found; pass --profile", file=sys.stderr)
            return 2
        token, where = find_token(roots)
        if not token:
            print("no session token in the profile — log in to agent.zhijuai.com first", file=sys.stderr)
            return 3
        payload = decode_payload(token)
        exp = payload.get("exp")
        left = ""
        if isinstance(exp, int):
            days = (exp - time.time()) / 86400
            left = f", expires in {days:.1f} days"
            if days <= 0:
                left = ", EXPIRED — log in again"
        if not args.quiet:
            print(f"token uid={payload.get('uid')} user={payload.get('username')}{left}")
            print(f"  from {where}")
        if args.push:
            if not args.gateway or not args.key:
                print("--push needs --gateway and --key", file=sys.stderr)
                return 4
            try:
                res = push(args.gateway, args.key, token)
                print(f"pushed: {json.dumps(res, ensure_ascii=False)[:300]}")
            except urllib.error.HTTPError as e:
                print(f"push failed: HTTP {e.code} {e.read()[:200]!r}", file=sys.stderr)
                return 5
            except Exception as e:  # noqa: BLE001
                print(f"push failed: {e}", file=sys.stderr)
                return 5
        return 0

    if not args.watch:
        return once()

    last = ""
    while True:
        code = once()
        if code != 0 and not args.quiet:
            print(f"(retry in {args.watch}s)", file=sys.stderr)
        time.sleep(max(30, args.watch))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
