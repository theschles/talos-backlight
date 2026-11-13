#!/usr/bin/env python3
"""Package a compiled Linux amd64 controller and export a Talos image cache."""

import contextlib
import gzip
import hashlib
import io
import json
import runpy
import sys
import tarfile
from pathlib import Path

binary, cache, release = map(Path, sys.argv[1:])
release.mkdir(parents=True, exist_ok=True)
layer = io.BytesIO()
with tarfile.open(fileobj=layer, mode="w") as archive:
    info = tarfile.TarInfo("backlight")
    info.mode = 0o555
    info.size = binary.stat().st_size
    with binary.open("rb") as source:
        archive.addfile(info, source)
raw = layer.getvalue()
config = {
    "architecture": "amd64",
    "os": "linux",
    "config": {"Entrypoint": ["/backlight"], "Cmd": ["serve"]},
    "rootfs": {"type": "layers", "diff_ids": ["sha256:" + hashlib.sha256(raw).hexdigest()]},
}
manifest = [
    {"Config": "config.json", "RepoTags": ["controller-inventory:v1"], "Layers": ["layer.tar.gz"]}
]
image = release / "controller-amd64.tar"
with tarfile.open(image, mode="w") as archive:
    for name, data in [
        ("config.json", json.dumps(config).encode()),
        ("layer.tar.gz", gzip.compress(raw, mtime=0)),
        ("manifest.json", json.dumps(manifest).encode()),
    ]:
        info = tarfile.TarInfo(name)
        info.size = len(data)
        archive.addfile(info, io.BytesIO(data))
with (release / "image-reference.txt").open("w") as output, contextlib.redirect_stdout(output):
    runpy.run_path(str(Path(__file__).with_name("prepare-cache.py")))["export"](
        image, cache, "controller-inventory", "v1"
    )
(release / "SHA256SUMS").write_text(
    hashlib.sha256(image.read_bytes()).hexdigest() + "  controller-amd64.tar\n"
)
