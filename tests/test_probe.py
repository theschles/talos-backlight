"""Test the read-only probe against fixture Talos responses."""

import contextlib
import io
import json
import runpy
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]


class ProbeTest(unittest.TestCase):
    def test_reports_unbound_gpu_and_requires_opt_in_for_catalog(self) -> None:
        module = runpy.run_path(str(ROOT / "probe-backlight"))
        report = module["report"]
        gpu = {
            "metadata": {"id": "0000:00:02.0"},
            "spec": {"class_id": "0x03", "vendor_id": "0x8086", "product_id": "0x5917"},
        }
        for check in (False, True):
            with self.subTest(check=check):
                read = Mock(
                    side_effect=["Client:\nServer:\n Tag: v1.14.2", json.dumps(gpu), "", "."]
                )
                catalog = Mock(return_value=io.BytesIO(b'[{"name":"siderolabs/i915"}]'))
                output = io.StringIO()
                with (
                    patch.dict(report.__globals__, {"read": read}),
                    patch("urllib.request.urlopen", catalog),
                    contextlib.redirect_stdout(output),
                ):
                    report(["talosctl", "-n", "192.0.2.10"], check)
                self.assertIn("0x8086:0x5917", output.getvalue())
                self.assertIn("driver: unbound", output.getvalue())
                self.assertIn("Backlight interfaces: none", output.getvalue())
                if check:
                    catalog.assert_called_once_with(
                        "https://factory.talos.dev/version/v1.14.2/extensions/official", timeout=10
                    )
                    self.assertIn("Extension to investigate: siderolabs/i915", output.getvalue())
                else:
                    catalog.assert_not_called()
                    self.assertNotIn("Extension to investigate", output.getvalue())
