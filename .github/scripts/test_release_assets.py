import json
import struct
import tempfile
import unittest
import warnings
import zipfile
from pathlib import Path

import release_assets as release

COMMIT = "a" * 40


def binary(target):
    data = bytearray(256)
    if target[0] == "linux":
        data[:6] = b"\x7fELF\x02\x01"
        struct.pack_into("<HH", data, 16, 3, 62 if target[1] == "amd64" else 183)
    elif target[0] == "darwin":
        data[:4] = b"\xcf\xfa\xed\xfe"
        struct.pack_into("<I", data, 4, 0x0100000C)
        struct.pack_into("<I", data, 12, 6)
    else:
        data[:2] = b"MZ"
        struct.pack_into("<I", data, 60, 128)
        data[128:132] = b"PE\x00\x00"
        struct.pack_into("<H", data, 132, 0x8664)
        struct.pack_into("<H", data, 150, 0x2000)
        struct.pack_into("<H", data, 152, 0x20B)
    return bytes(data)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root / "assets"
        for name in release.DOCUMENTS:
            (self.root / name).write_text("public " + name, encoding="utf-8")

    def pack(self, target=("linux", "amd64")):
        library = self.root / release.library_name(target)
        library.write_bytes(binary(target))
        return release.pack(self.root, library, self.assets, "0.3.32", target, COMMIT,
                            "go version go1.26.10 linux/amd64")

    def rewrite(self, path, changes):
        with zipfile.ZipFile(path) as archive:
            files = {name: archive.read(name) for name in archive.namelist()}
        for key, value in changes.items():
            if value is None:
                files.pop(key, None)
            else:
                files[key] = value
        with zipfile.ZipFile(path, "w") as archive:
            for name, data in files.items():
                archive.writestr(name, data)

    def verify(self, path):
        return release.verify_archive(self.root, path, "0.3.32", ("linux", "amd64"), COMMIT)

    def test_all_native_formats_and_complete_release_round_trip(self):
        for target in release.PLATFORMS:
            self.pack(target)
        result = release.verify_set(self.root, self.assets, "0.3.32", COMMIT, True)
        self.assertEqual(len(result["assets"]), 4)
        self.assertEqual(release.verify_set(self.root, self.assets, "0.3.32", COMMIT), result)

    def test_pack_is_byte_deterministic(self):
        path = self.pack()
        before = path.read_bytes()
        self.pack()
        self.assertEqual(before, path.read_bytes())

    def test_windows_checkout_line_endings_produce_identical_documents(self):
        for name in release.DOCUMENTS:
            (self.root / name).write_bytes(("public " + name + "\r\n").encode())
        path = self.pack()
        before = path.read_bytes()
        for name in release.DOCUMENTS:
            (self.root / name).write_bytes(("public " + name + "\n").encode())
        self.assertEqual(self.verify(path)["source_commit"], COMMIT)
        self.pack()
        self.assertEqual(before, path.read_bytes())

    def test_wrong_architecture_and_executable_are_rejected(self):
        for target in release.PLATFORMS:
            with self.subTest(target=target), self.assertRaises(ValueError):
                release.verify_binary(b"MZ" + bytes(254) if target[0] != "windows" else binary(("linux", "amd64")), target)
        with self.assertRaisesRegex(ValueError, "architecture"):
            release.verify_binary(binary(("linux", "arm64")), ("linux", "amd64"))
        executable = bytearray(binary(("windows", "amd64")))
        struct.pack_into("<H", executable, 150, 0)
        with self.assertRaisesRegex(ValueError, "not a DLL"):
            release.verify_binary(executable, ("windows", "amd64"))

    def test_library_tampering_is_rejected(self):
        path = self.pack()
        self.rewrite(path, {"codex-quota-scheduler.so": binary(("linux", "amd64")) + b"tampered"})
        with self.assertRaisesRegex(ValueError, "library_sha256"):
            self.verify(path)

    def test_wrong_source_commit_is_rejected(self):
        path = self.pack()
        with self.assertRaisesRegex(ValueError, "source_commit"):
            release.verify_archive(self.root, path, "0.3.32", ("linux", "amd64"), "b" * 40)

    def test_missing_or_modified_license_is_rejected(self):
        for value in [None, b"wrong license"]:
            path = self.pack()
            self.rewrite(path, {"LICENSE": value})
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.verify(path)

    def test_unexpected_nested_and_private_files_are_rejected(self):
        for name in ["../private", "nested/readme", "settings.json"]:
            path = self.pack()
            self.rewrite(path, {name: b"unexpected"})
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.verify(path)

    def test_duplicate_library_is_rejected(self):
        path = self.pack()
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", UserWarning)
            with zipfile.ZipFile(path, "a") as archive:
                archive.writestr("codex-quota-scheduler.so", binary(("linux", "amd64")))
        with self.assertRaisesRegex(ValueError, "exact root"):
            self.verify(path)

    def test_incomplete_release_cannot_generate_checksums(self):
        self.pack()
        with self.assertRaisesRegex(ValueError, "four platform"):
            release.verify_set(self.root, self.assets, "0.3.32", COMMIT, True)

    def test_checksum_and_verification_document_tampering_are_rejected(self):
        for target in release.PLATFORMS:
            self.pack(target)
        release.verify_set(self.root, self.assets, "0.3.32", COMMIT, True)
        path = self.assets / "checksums.txt"
        path.write_text(path.read_text() + path.read_text().splitlines()[0] + "\n")
        with self.assertRaises(ValueError):
            release.verify_set(self.root, self.assets, "0.3.32", COMMIT)
        release.verify_set(self.root, self.assets, "0.3.32", COMMIT, True)
        path = self.assets / "release-verification.json"
        d = json.loads(path.read_text())
        d["source_commit"] = "b" * 40
        path.write_text(json.dumps(d))
        with self.assertRaisesRegex(ValueError, "verification manifest"):
            release.verify_set(self.root, self.assets, "0.3.32", COMMIT)


if __name__ == "__main__":
    unittest.main()
