"""Exercise image validation and cache output boundaries."""

import hashlib
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class ImageCacheTest(unittest.TestCase):
    """Verify the exporter through its command-line interface."""

    def test_optimized_oci_export_writes_and_validates_blobs(self) -> None:
        blob = b"{}"
        blob_hash = hashlib.sha256(blob).hexdigest()
        manifest = json.dumps(
            {
                "config": {
                    "digest": f"sha256:{blob_hash}",
                    "mediaType": "application/vnd.oci.image.config.v1+json",
                    "size": len(blob),
                },
                "layers": [],
            }
        ).encode()
        manifest_hash = hashlib.sha256(manifest).hexdigest()
        index = json.dumps({"manifests": [{"digest": f"sha256:{manifest_hash}"}]}).encode()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "image.tar"
            command = [
                sys.executable,
                "-O",
                str(ROOT / "prepare-cache.py"),
                str(archive),
                str(root / "cache"),
                "--repository",
                "nested/controller_2",
                "--tag",
                "Version-1",
            ]
            for content in (blob, b"tampered"):
                with tarfile.open(archive, "w") as tar:
                    for name, data in (
                        ("index.json", index),
                        (f"blobs/sha256/{manifest_hash}", manifest),
                        (f"blobs/sha256/{blob_hash}", content),
                    ):
                        member = tarfile.TarInfo(name)
                        member.size = len(data)
                        tar.addfile(member, io.BytesIO(data))
                result = subprocess.run(command, text=True, capture_output=True, timeout=10)
                if content == blob:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual((root / f"cache/blob/sha256-{blob_hash}").read_bytes(), blob)
                    self.assertEqual(
                        (
                            root
                            / "cache/manifests/gmux.invalid/nested/controller_2/reference/Version-1"
                        ).read_bytes(),
                        manifest,
                    )
                    self.assertEqual(
                        result.stdout.strip(),
                        f"gmux.invalid/nested/controller_2@sha256:{manifest_hash}",
                    )
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("OCI blob descriptor mismatch", result.stderr)

    def test_export_rejects_unsafe_reference_paths(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = self.docker_archive(root)
            for repository, tag in (
                (str(root / "outside"), "local"),
                ("../../../outside", "local"),
                ("nested//controller", "local"),
                ("UPPER", "local"),
                ("controller", str(root / "outside")),
                ("controller", "../outside"),
                ("controller", "."),
                ("controller", "a" * 129),
            ):
                with self.subTest(repository=repository, tag=tag):
                    result = subprocess.run(
                        [
                            sys.executable,
                            str(ROOT / "prepare-cache.py"),
                            str(archive),
                            str(root / "cache"),
                            "--repository",
                            repository,
                            "--tag",
                            tag,
                        ],
                        text=True,
                        capture_output=True,
                        timeout=10,
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertRegex(result.stderr, r"invalid (repository|tag)")
                    self.assertFalse((root / "cache").exists())
                    self.assertFalse((root / "outside").exists())

    @staticmethod
    def docker_archive(root: Path) -> Path:
        """Create a small Docker archive in a temporary directory."""
        archive = root / "image.tar"
        with tarfile.open(archive, "w") as tar:
            for name, data in (
                ("manifest.json", b'[{"Config":"config.json","Layers":[]}]'),
                ("config.json", b"{}"),
            ):
                member = tarfile.TarInfo(name)
                member.size = len(data)
                tar.addfile(member, io.BytesIO(data))
        return archive

    def test_export_refuses_symlinks_that_escape_cache(self) -> None:
        blob_hash = hashlib.sha256(b"{}").hexdigest()
        for relative_path in (
            "manifests",
            "manifests/gmux.invalid/controller-inventory/reference/local",
            "blob",
            f"blob/sha256-{blob_hash}",
        ):
            with self.subTest(path=relative_path), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = self.docker_archive(root)
                cache = root / "cache"
                outside = root / "outside"
                link = cache / relative_path
                if relative_path in ("manifests", "blob"):
                    outside.mkdir()
                    sentinel = outside / "sentinel"
                else:
                    sentinel = outside
                sentinel.write_bytes(b"keep this")
                link.parent.mkdir(parents=True)
                link.symlink_to(outside)
                result = subprocess.run(
                    [sys.executable, str(ROOT / "prepare-cache.py"), str(archive), str(cache)],
                    text=True,
                    capture_output=True,
                    timeout=10,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("output path escapes cache", result.stderr)
                self.assertEqual(sentinel.read_bytes(), b"keep this")
                if outside.is_dir():
                    self.assertEqual(list(outside.iterdir()), [sentinel])

    def test_export_replaces_hard_links_without_overwriting_outside_file(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = self.docker_archive(root)
            cache = root / "cache"
            blob_hash = hashlib.sha256(b"{}").hexdigest()
            paths = (
                cache / f"blob/sha256-{blob_hash}",
                cache / "manifests/gmux.invalid/controller-inventory/reference/local",
            )
            outside = root / "outside"
            outside.write_bytes(b"keep this")
            for path in paths:
                path.parent.mkdir(parents=True)
                os.link(outside, path)
            result = subprocess.run(
                [sys.executable, str(ROOT / "prepare-cache.py"), str(archive), str(cache)],
                text=True,
                capture_output=True,
                timeout=10,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(outside.read_bytes(), b"keep this")
            self.assertEqual(paths[0].read_bytes(), b"{}")
            self.assertEqual(json.loads(paths[1].read_bytes())["schemaVersion"], 2)
