"""Package and verify the exact CPA release asset set using only the stdlib."""

import argparse
import hashlib
import json
import re
import struct
import subprocess
import zipfile
from pathlib import Path

PLUGIN = "codex-quota-scheduler"
PLATFORMS = {("linux", "amd64"): "so", ("linux", "arm64"): "so",
             ("darwin", "arm64"): "dylib", ("windows", "amd64"): "dll"}
DOCUMENTS = ("LICENSE", "NOTICE", "CPA_PERMISSIONS.md", "INSTALL.zh-CN.md",
             "SERIAL_CONFIG.example.yaml")
MANIFEST = "release-manifest.json"
MAX_LIBRARY_BYTES = 128 * 1024 * 1024


def require(condition, message):
    if not condition:
        raise ValueError(message)


def asset_name(version, target):
    require(re.fullmatch(r"\d+\.\d+\.\d+", version), "invalid release version")
    require(target in PLATFORMS, "unsupported release platform")
    return f"{PLUGIN}_{version}_{target[0]}_{target[1]}.zip"


def library_name(target):
    require(target in PLATFORMS, "unsupported library platform")
    return f"{PLUGIN}.{PLATFORMS[target]}"


def verify_binary(data, target):
    """Read native headers; a renamed or wrong-architecture library must fail."""
    require(64 <= len(data) <= MAX_LIBRARY_BYTES, "invalid library size")
    if target[0] == "linux":
        require(data[:6] == b"\x7fELF\x02\x01", "expected little-endian ELF64")
        kind, machine = struct.unpack_from("<HH", data, 16)
        require(kind == 3, "expected ELF shared library")
        require(machine == {"amd64": 62, "arm64": 183}[target[1]], "ELF architecture mismatch")
    elif target == ("windows", "amd64"):
        require(data[:2] == b"MZ", "expected PE library")
        offset = struct.unpack_from("<I", data, 60)[0]
        require(offset + 26 <= len(data), "truncated PE header")
        require(data[offset:offset + 4] == b"PE\x00\x00", "invalid PE signature")
        require(struct.unpack_from("<H", data, offset + 4)[0] == 0x8664, "PE architecture mismatch")
        require(struct.unpack_from("<H", data, offset + 22)[0] & 0x2000, "PE is not a DLL")
        require(struct.unpack_from("<H", data, offset + 24)[0] == 0x20B, "expected PE32+")
    elif target == ("darwin", "arm64"):
        require(data[:4] == b"\xcf\xfa\xed\xfe", "expected little-endian Mach-O64")
        require(struct.unpack_from("<I", data, 4)[0] == 0x0100000C, "Mach-O architecture mismatch")
        require(struct.unpack_from("<I", data, 12)[0] == 6, "Mach-O is not a dylib")
    else:
        raise ValueError("unsupported binary platform")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def document(root, name):
    # Git may check text out as CRLF on Windows. Ship identical UTF-8/LF docs
    # so the release verifier on Linux checks the same contents on every OS.
    return (root / name).read_text(encoding="utf-8").encode("utf-8")


def verify_commit(commit):
    require(isinstance(commit, str) and re.fullmatch(r"[0-9a-f]{40}", commit),
            "expected exact source commit")


def pack(root, library, directory, version, target, commit, toolchain):
    name = asset_name(version, target)
    verify_commit(commit)
    data = library.read_bytes()
    verify_binary(data, target)
    require(re.search(r"\bgo1\.\d+\.\d+\b", toolchain), "missing Go toolchain version")
    manifest = {"schema_version": 1, "plugin_id": PLUGIN, "version": version,
                "goos": target[0], "goarch": target[1], "source_commit": commit,
                "go_toolchain": toolchain, "library": library_name(target),
                "library_sha256": digest(data), "library_bytes": len(data)}
    files = {library_name(target): data}
    files.update({name: document(root, name) for name in DOCUMENTS})
    files[MANIFEST] = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / name
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for filename, contents in sorted(files.items()):
            info = zipfile.ZipInfo(filename, date_time=(1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = 0o100644 << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, contents, compresslevel=9)
    verify_archive(root, path, version, target, commit)
    return path


def verify_archive(root, path, version, target, commit):
    require(path.name == asset_name(version, target), "archive filename mismatch")
    verify_commit(commit)
    expected = {library_name(target), MANIFEST, *DOCUMENTS}
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        require(len(names) == len(expected) and set(names) == expected,
                "archive must contain the exact root library and public documents")
        for info in archive.infolist():
            require(not info.flag_bits & 1, "encrypted archive entry")
            require(info.file_size <= MAX_LIBRARY_BYTES, "oversized archive entry")
            require((info.external_attr >> 16) & 0o170000 in (0, 0o100000), "non-regular archive entry")
        data = archive.read(library_name(target))
        verify_binary(data, target)
        for name in DOCUMENTS:
            require(archive.read(name) == document(root, name), f"document differs from source: {name}")
        manifest = json.loads(archive.read(MANIFEST))
        expected_values = {"schema_version": 1, "plugin_id": PLUGIN, "version": version,
                           "goos": target[0], "goarch": target[1], "source_commit": commit,
                           "library": library_name(target), "library_sha256": digest(data),
                           "library_bytes": len(data)}
        require(set(manifest) == {*expected_values, "go_toolchain"}, "unexpected manifest fields")
        for key, value in expected_values.items():
            require(manifest[key] == value, f"manifest mismatch: {key}")
        require(isinstance(manifest["go_toolchain"], str) and
                re.search(r"\bgo1\.\d+\.\d+\b", manifest["go_toolchain"]), "missing Go toolchain")
    return manifest


def verify_set(root, directory, version, commit, write_checksums=False):
    expected = {asset_name(version, target): target for target in PLATFORMS}
    require({p.name for p in directory.glob("*.zip")} == set(expected), "expected all four platform archives")
    manifests = {name: verify_archive(root, directory / name, version, target, commit)
                 for name, target in sorted(expected.items())}
    checksums = {name: digest((directory / name).read_bytes()) for name in sorted(expected)}
    checksum_path = directory / "checksums.txt"
    if write_checksums:
        checksum_path.write_text("".join(f"{checksum}  {name}\n" for name, checksum in checksums.items()),
                                 encoding="utf-8", newline="\n")
    parsed = {}
    for line in checksum_path.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([a-zA-Z0-9_.-]+)", line)
        require(match is not None, "invalid checksum entry")
        checksum, name = match.groups()
        require(name not in parsed, "duplicate checksum entry")
        parsed[name] = checksum
    require(parsed == checksums, "checksum set or digest mismatch")
    result = {"schema_version": 1, "plugin_id": PLUGIN, "version": version,
              "source_commit": commit, "assets": [
                  {"name": name, "archive_sha256": checksums[name], **manifest}
                  for name, manifest in manifests.items()]}
    if write_checksums:
        (directory / "release-verification.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    else:
        require(json.loads((directory / "release-verification.json").read_text()) == result,
                "published verification manifest mismatch")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("pack", "verify-set"))
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--os", choices=("linux", "windows", "darwin"))
    parser.add_argument("--arch", choices=("amd64", "arm64"))
    parser.add_argument("--library", type=Path)
    parser.add_argument("--write-checksums", action="store_true")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    if args.operation == "pack":
        require(args.library is not None, "library path required")
        toolchain = subprocess.check_output(["go", "version"], text=True).strip()
        print(pack(root, args.library, args.assets, args.version,
                   (args.os, args.arch), args.source_commit, toolchain).name)
    else:
        result = verify_set(root, args.assets, args.version, args.source_commit, args.write_checksums)
        print(f"Verified {len(result['assets'])} platform archives for {args.version} at {args.source_commit}")


if __name__ == "__main__":
    main()
