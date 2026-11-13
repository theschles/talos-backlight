#!/usr/bin/env python3
"""Convert a local image archive to Talos image-cache layout."""

import hashlib
import json
import os
import re
import tarfile
import tempfile
from pathlib import Path
from typing import TypedDict

MANIFEST = "application/vnd.oci.image.manifest.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
REPOSITORY_COMPONENT = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
REPOSITORY = re.compile(rf"{REPOSITORY_COMPONENT}(?:/{REPOSITORY_COMPONENT})*")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")


class Descriptor(TypedDict):
    """OCI descriptor fields used to address and verify an image blob."""

    mediaType: str
    digest: str
    size: int


def descriptor(data: bytes, media: str) -> Descriptor:
    """Describe bytes for the manifest and content-addressed cache."""
    return {
        "mediaType": media,
        "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
        "size": len(data),
    }


def write_cache_file(path: Path, data: bytes) -> None:
    """Publish a complete cache entry without writing through an existing hard link."""
    path.parent.mkdir(parents=True, exist_ok=True)
    file = tempfile.NamedTemporaryFile(dir=path.parent, delete=False)
    temporary = Path(file.name)
    try:
        with file:
            file.write(data)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def export(source: Path, root: Path, repository: str, tag: str) -> None:
    """Store verified image blobs and print the digest-pinned reference."""
    if len(repository) > 255 or not REPOSITORY.fullmatch(repository):
        raise ValueError(f"invalid repository: {repository!r}")
    if not TAG.fullmatch(tag):
        raise ValueError(f"invalid tag: {tag!r}")
    root = root.resolve()

    def cache_path(path: Path) -> Path:
        resolved = path.resolve()
        if resolved == root or not resolved.is_relative_to(root):
            raise ValueError(f"output path escapes cache: {path}")
        return resolved

    directory = root / "manifests" / "gmux.invalid" / repository
    reference = cache_path(directory / "reference" / tag)
    blob_directory = cache_path(root / "blob")
    with tarfile.open(source, "r") as archive:

        def read(name: str) -> bytes:
            member = archive.extractfile(name)
            if member is None:
                raise ValueError(f"image archive member is not a file: {name}")
            with member:
                return member.read()

        def save_blob(data: bytes, media: str) -> Descriptor:
            desc = descriptor(data, media)
            path = cache_path(blob_directory / desc["digest"].replace(":", "-"))
            write_cache_file(path, data)
            return desc

        if "index.json" in archive.getnames():
            index = json.loads(read("index.json"))
            (desc,) = index["manifests"]
            manifest = read("blobs/" + desc["digest"].replace(":", "/"))
            if descriptor(manifest, MANIFEST)["digest"] != desc["digest"]:
                raise ValueError("OCI manifest digest mismatch")
            image = json.loads(manifest)
            for blob in (image["config"], *image["layers"]):
                data = read("blobs/" + blob["digest"].replace(":", "/"))
                if descriptor(data, blob["mediaType"]) != blob:
                    raise ValueError(f"OCI blob descriptor mismatch: {blob['digest']}")
                save_blob(data, blob["mediaType"])
        else:
            (image,) = json.loads(read("manifest.json"))
            config = save_blob(read(image["Config"]), CONFIG)
            layers = []
            for member in image["Layers"]:
                data = read(member)
                if not data.startswith(b"\x1f\x8b"):
                    raise ValueError(f"image layer is not gzip compressed: {member}")
                layers.append(save_blob(data, LAYER))
            manifest = json.dumps(
                {"schemaVersion": 2, "mediaType": MANIFEST, "config": config, "layers": layers},
                separators=(",", ":"),
            ).encode()

    digest = descriptor(manifest, MANIFEST)["digest"]
    digest_path = cache_path(directory / "digest" / digest.replace(":", "-"))
    for path in (digest_path, reference):
        write_cache_file(path, manifest)
    print(f"gmux.invalid/{repository}@{digest}")


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(
        description="Export a local image archive to a Talos image cache."
    )
    parser.add_argument("source", type=Path)
    parser.add_argument("cache", type=Path)
    parser.add_argument("--repository", default="controller-inventory")
    parser.add_argument("--tag", default="local")
    args = parser.parse_args()
    try:
        export(args.source, args.cache, args.repository, args.tag)
    except (OSError, ValueError, tarfile.TarError) as err:
        parser.exit(1, f"{parser.prog}: {err}\n")
