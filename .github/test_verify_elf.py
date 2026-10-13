#!/usr/bin/env python3
"""Self-test for verify_elf.py.

The verifier is the gate that keeps a broken binary out of a release, so it gets tested
the same way as the code it guards: synthetic ELF headers for each case we care about
(64-bit vs 32-bit, soft vs hard float, static vs dynamic), asserting which ones pass.
"""
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
VERIFY = HERE / "verify_elf.py"


def build_elf32(machine: int, fp_abi: int, little: bool = True) -> bytes:
    end = "<" if little else ">"
    shstr = b"\x00.shstrtab\x00.MIPS.abiflags\x00"
    abiflags = bytes([0, 0, 0, 0, 4, 4, 4, fp_abi, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0])

    ehsize, shentsize = 52, 40
    # layout: ehdr | shstrtab | abiflags | shdrs
    shstr_off = ehsize
    abi_off = shstr_off + len(shstr)
    shoff = abi_off + len(abiflags)

    ident = b"\x7fELF" + bytes([1, 1 if little else 2, 1, 0]) + b"\x00" * 8
    # e_type, e_machine, e_version, e_entry, e_phoff, e_shoff, e_flags,
    # e_ehsize, e_phentsize, e_phnum, e_shentsize, e_shnum, e_shstrndx
    ehdr = ident + struct.pack(end + "HHIIIIIHHHHHH",
                               2, machine, 1, 0, 0,
                               shoff,
                               0, ehsize,
                               0, 0,        # e_phentsize, e_phnum -> no program headers
                               shentsize, 3, 1)

    def shdr(name, typ, off, size):
        return struct.pack(end + "IIIIIIIIII", name, typ, 0, 0, off, size, 0, 0, 4, 0)

    shdrs = shdr(0, 0, 0, 0)
    shdrs += shdr(1, 3, shstr_off, len(shstr))              # .shstrtab
    shdrs += shdr(11, 0x70000003, abi_off, len(abiflags))   # .MIPS.abiflags
    return ehdr + shstr + abiflags + shdrs


def build_elf64(machine: int, interp: str | None) -> bytes:
    end = "<"
    phoff = 64
    phentsize, phnum = 56, 1 if interp else 0
    interp_bytes = (interp.encode() + b"\x00") if interp else b""
    interp_off = phoff + phentsize * max(phnum, 1)

    ident = b"\x7fELF" + bytes([2, 1, 1, 0]) + b"\x00" * 8
    ehdr = ident + struct.pack(end + "HHIQQQIHHHHHH",
                               2, machine, 1, 0, phoff, 0, 0, 64,
                               phentsize, phnum, 64, 0, 0)
    phdrs = b""
    if interp:
        phdrs = struct.pack(end + "IIQQQQQQ", 3, 4, interp_off, 0, 0, len(interp_bytes), 0, 0)
    return ehdr + phdrs + interp_bytes


CASES = [
    ("mipsle softfloat (the router)", build_elf32(0x08, 3), "mipsle", "softfloat", True),
    ("mipsle hardfloat must be rejected", build_elf32(0x08, 0), "mipsle", "softfloat", False),
    ("mips big endian softfloat", build_elf32(0x08, 3, little=False), "mips", "softfloat", True),
    ("armv7 (32-bit, no MIPS abiflags)", build_elf32(0x28, 0), "arm", "", True),
    ("amd64 static", build_elf64(0x3E, None), "amd64", "", True),
    ("arm64 static", build_elf64(0xB7, None), "arm64", "", True),
    ("amd64 dynamic must be rejected", build_elf64(0x3E, "/lib64/ld-linux-x86-64.so.2"), "amd64", "", False),
    ("arm64 dynamic must be rejected", build_elf64(0xB7, "/lib/ld-linux-aarch64.so.1"), "arm64", "", False),
    ("ELF64 on a 32-bit target must be rejected", build_elf64(0x08, None), "mipsle", "softfloat", False),
]


def main() -> int:
    failures = 0
    with tempfile.TemporaryDirectory() as tmp:
        for label, blob, goarch, gomips, want_ok in CASES:
            path = Path(tmp) / f"case-{abs(hash(label))}.bin"
            path.write_bytes(blob)
            args = [sys.executable, str(VERIFY), str(path)]
            if goarch:
                args.append(goarch)
            args.append(gomips)
            proc = subprocess.run(args, capture_output=True, text=True)
            ok = proc.returncode == 0
            verdict = "PASS" if ok == want_ok else "MISMATCH"
            if ok != want_ok:
                failures += 1
            print(f"[{verdict}] {label}: expected {'accept' if want_ok else 'reject'}, got {'accept' if ok else 'reject'}")
            if ok != want_ok:
                print("   stdout:", proc.stdout.strip().replace("\n", " | "))
                print("   stderr:", proc.stderr.strip().replace("\n", " | "))
    print(f"\n{len(CASES) - failures}/{len(CASES)} cases behave as expected")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
