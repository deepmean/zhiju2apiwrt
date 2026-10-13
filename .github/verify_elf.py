#!/usr/bin/env python3
"""Assert a built binary matches what the target device can actually execute.

A wrong FP ABI is the nasty one: the binary starts fine and then dies the first time
it touches a float, which on a router looks like a random crash rather than a build
mistake. Checking the ELF header in CI is what keeps that class of bug out of a
release.
"""
import struct
import sys

EM = {0x08: "MIPS", 0x28: "ARM", 0xB7: "AARCH64", 0x3E: "X86-64"}
EF_MIPS_ABI_FLOAT_SOFT = 3


def fail(msg: str) -> "None":
    print(f"FAIL: {msg}")
    sys.exit(1)


def main() -> int:
    if len(sys.argv) < 2:
        fail("usage: verify_elf.py <binary> [goarch] [gomips]")
    path = sys.argv[1]
    goarch = sys.argv[2] if len(sys.argv) > 2 else ""
    gomips = sys.argv[3] if len(sys.argv) > 3 else ""

    with open(path, "rb") as f:
        head = f.read(64)

    if head[:4] != b"\x7fELF":
        fail(f"{path}: not an ELF file")
    if head[4] != 1:
        fail(f"{path}: not ELF32 (this device is 32-bit)")

    little = head[5] == 1
    machine = struct.unpack("<H" if little else ">H", head[18:20])[0]
    name = EM.get(machine, hex(machine))

    print(f"{path}: ELF32, {'little' if little else 'big'} endian, machine={name}")

    if goarch in ("mipsle", "mips"):
        if machine != 0x08:
            fail(f"expected MIPS, got {name}")
        if goarch == "mipsle" and not little:
            fail("expected little endian for mipsle")
        if goarch == "mips" and little:
            fail("expected big endian for mips")
        # .MIPS.abiflags: fp_abi lives at offset 12 of the section payload; the loader
        # writes 3 for soft float. Read the section header table to find it.
        with open(path, "rb") as f:
            data = f.read()
        shoff = struct.unpack("<I" if little else ">I", data[32:36])[0]
        shentsize = struct.unpack("<H" if little else ">H", data[46:48])[0]
        shnum = struct.unpack("<H" if little else ">H", data[48:50])[0]
        shstrndx = struct.unpack("<H" if little else ">H", data[50:52])[0]

        def sh(i):
            off = shoff + i * shentsize
            return struct.unpack("<IIIIIIIIII" if little else ">IIIIIIIIII", data[off:off + 40])

        # ELF32 section header: name, type, flags, addr, offset, size, link, info,
        # addralign, entsize — the string table's file offset is index 4.
        _, _, _, _, strtab_off, _, _, _, _, _ = sh(shstrndx)
        sections = [sh(i) for i in range(shnum)]
        for s in sections:
            nm_off = s[0]
            end = data.index(b"\x00", strtab_off + nm_off)
            sname = data[strtab_off + nm_off:end].decode("ascii", "replace")
            if sname == ".MIPS.abiflags":
                s_off = s[4]
                # .MIPS.abiflags: version(2) isa_level(1) isa_rev(1) gpr_size(1)
                # cpr1_size(1) cpr2_size(1) fp_abi(1) ...
                fp_abi = data[s_off + 7]
                print(f"  .MIPS.abiflags fp_abi={fp_abi}")
                if gomips == "softfloat" and fp_abi != EF_MIPS_ABI_FLOAT_SOFT:
                    fail(f"expected soft float (fp_abi=3), got {fp_abi} — this binary would crash on a FPU-less router")
                break
        else:
            fail(".MIPS.abiflags section not found; cannot prove the FP ABI")

    # A static binary is required: there is no dynamic loader for these arches on OpenWrt.
    with open(path, "rb") as f:
        data = f.read()
    if b"/lib/ld" in data[:0x2000] or b"ld-musl" in data[:0x2000]:
        fail("looks dynamically linked; expected a static binary")

    print("OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
