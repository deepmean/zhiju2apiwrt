#!/usr/bin/env python3
"""Assert a built binary matches what the target device can actually execute.

Two checks earn their keep here:

* **FP ABI.** A hard-float MIPS binary starts fine and then dies the first time it
  touches a float — on a router that looks like a random crash, not a build mistake.
  `.MIPS.abiflags` says which ABI the linker emitted, so we read it.
* **Static linking.** OpenWrt has no dynamic loader for these arches, so a `PT_INTERP`
  program header means the binary will never start. Checking the header beats grepping
  for "/lib/ld" in the first pages of the file, which both misses real interpreters and
  matches unrelated strings.

Usage: verify_elf.py <binary> [goarch] [gomips]
"""
import struct
import sys

MACHINE = {0x03: "i386", 0x08: "MIPS", 0x28: "ARM", 0x3E: "x86-64", 0xB7: "AArch64"}
EM_MIPS_ABI_FLOAT_SOFT = 3
PT_INTERP = 3

# GOARCH values that must produce a 64-bit ELF; everything else we build is 32-bit.
ARCH64 = {"amd64", "arm64", "riscv64", "s390x", "ppc64le", "loong64"}


def fail(msg: str) -> None:
    print(f"FAIL: {msg}")
    sys.exit(1)


def main() -> int:
    if len(sys.argv) < 2:
        fail("usage: verify_elf.py <binary> [goarch] [gomips]")
    path = sys.argv[1]
    goarch = sys.argv[2] if len(sys.argv) > 2 else ""
    gomips = sys.argv[3] if len(sys.argv) > 3 else ""

    with open(path, "rb") as f:
        data = f.read()

    if data[:4] != b"\x7fELF":
        fail(f"{path}: not an ELF file")

    is64 = data[4] == 2
    little = data[5] == 1
    machine = struct.unpack("<H" if little else ">H", data[18:20])[0]
    name = MACHINE.get(machine, hex(machine))
    print(f"{path}: ELF{'64' if is64 else '32'}, {'little' if little else 'big'} endian, machine={name}")

    if goarch:
        want64 = goarch in ARCH64
        if want64 and not is64:
            fail(f"goarch={goarch} must produce ELF64, got ELF32")
        if not want64 and is64:
            fail(f"goarch={goarch} must produce ELF32, got ELF64")

    # --- static linking: no PT_INTERP ------------------------------------------------
    if is64:
        phoff = struct.unpack("<Q" if little else ">Q", data[32:40])[0]
        phentsize = struct.unpack("<H" if little else ">H", data[54:56])[0]
        phnum = struct.unpack("<H" if little else ">H", data[56:58])[0]
        fmt, width = ("<IIQQQQQQ" if little else ">IIQQQQQQ"), 56
    else:
        phoff = struct.unpack("<I" if little else ">I", data[28:32])[0]
        phentsize = struct.unpack("<H" if little else ">H", data[42:44])[0]
        phnum = struct.unpack("<H" if little else ">H", data[44:46])[0]
        fmt, width = ("<IIIIIIII" if little else ">IIIIIIII"), 32

    interp = None
    for i in range(phnum):
        off = phoff + i * phentsize
        if off + width > len(data):
            break
        ph = struct.unpack(fmt, data[off:off + width])
        if ph[0] == PT_INTERP:
            interp = data[ph[2]:ph[2] + ph[5]].rstrip(b"\x00").decode("ascii", "replace")
    if interp:
        fail(f"dynamically linked (PT_INTERP={interp!r}); the target has no such loader")
    print("  no PT_INTERP -> statically linked")

    # --- MIPS specifics ---------------------------------------------------------------
    if goarch in ("mipsle", "mips") or machine == 0x08:
        if machine != 0x08:
            fail(f"expected MIPS, got {name}")
        if goarch == "mipsle" and not little:
            fail("expected little endian for mipsle")
        if goarch == "mips" and little:
            fail("expected big endian for mips")

        if is64:
            fail("MIPS64 is not a target of this build matrix")

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

        found = False
        for i in range(shnum):
            s = sh(i)
            nm_off = s[0]
            end = data.index(b"\x00", strtab_off + nm_off)
            sname = data[strtab_off + nm_off:end].decode("ascii", "replace")
            if sname != ".MIPS.abiflags":
                continue
            found = True
            # version(2) isa_level(1) isa_rev(1) gpr_size(1) cpr1_size(1) cpr2_size(1)
            # fp_abi(1) isa_ext(4) ases(4) flags1(4) flags2(4)
            fp_abi = data[s[4] + 7]
            print(f"  .MIPS.abiflags fp_abi={fp_abi}")
            if gomips == "softfloat" and fp_abi != EM_MIPS_ABI_FLOAT_SOFT:
                fail(f"expected soft float (fp_abi=3), got {fp_abi} — this binary would crash on an FPU-less router")
        if not found:
            fail(".MIPS.abiflags section not found; cannot prove the FP ABI")

    print("OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
