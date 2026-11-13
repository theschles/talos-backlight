"""Exercise portability and command routing without a live Kubernetes cluster."""

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "gmux.invalid/controller-inventory@sha256:" + "1" * 64
STUB = """#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
import yaml
args = sys.argv[1:]
with open(os.environ["CALL_LOG"], "a") as log:
    log.write(json.dumps([Path(sys.argv[0]).name, *args]) + "\\n")
if Path(sys.argv[0]).name == "talosctl":
    if "machineconfig" in args:
        spec = os.environ.get("MACHINE_CONFIG", "apiVersion: v1alpha1\\nkind: MachineConfig\\n")
        print(json.dumps({"metadata":{"id":"v1alpha1"}, "spec":spec}))
    elif "ls" in args:
        print("192.0.2.10 /sys/class/backlight/gmux_backlight")
    elif args[-1] == "/etc/machine-id":
        print(os.environ.get("TALOS_MACHINE_ID", "a" * 32))
    elif args[-1].endswith("/type"):
        print("platform")
    elif args[-1].endswith("/max_brightness"):
        print("82311")
elif "namespace" in args and "get" in args and os.environ.get("UNOWNED_NAMESPACE"):
    print(json.dumps({"metadata":{"uid":"namespace-fixture","resourceVersion":"1","labels":{"pod-security.kubernetes.io/enforce":"privileged"}}}))
elif any(arg.startswith("daemonsets,") for arg in args):
    print(json.dumps({"items":[]}))
elif "nodes" in args:
    print(json.dumps({"items":[{"metadata":{"name":"control-plane","uid":"fixture-control-plane","labels":{"node-role.kubernetes.io/control-plane":""}},"status":{"nodeInfo":{"machineID":"a"*32},"addresses":[{"type":"InternalIP","address":"192.0.2.10"}]}}]}))
elif "pods" in args:
    print(json.dumps({"items":[{"metadata":{"name":name},"spec":{"nodeName":node},"status":{"phase":phase,"containerStatuses":[{"name":"backlight","ready":True}]}} for name,node,phase in [("pod-a","control-plane","Running"),("pod-b","worker","Running"),("pod-old","stopped","Succeeded")]]}))
elif "create" in args:
    print(json.dumps(yaml.safe_load(Path(args[args.index("-f")+1]).read_text())))
elif "exec" in args:
    if "pod/pod-a" in args and os.environ.get("FAIL_FIRST"):
        sys.exit(1)
    print("command accepted")
"""


class CLITest(unittest.TestCase):
    """Check user-facing routing and preview without cluster access."""

    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        for name in ("kubectl", "talosctl"):
            path = self.directory / name
            path.write_text(STUB)
            path.chmod(0o755)
        self.log = self.directory / "calls.jsonl"
        self.inventory = self.directory / "nodes.json"
        self.inventory.write_text(
            json.dumps({"nodes": [{"ip": "192.0.2.10", "nickname": "laptop-a"}]})
        )
        self.env = {
            key: value
            for key, value in os.environ.items()
            if not key.startswith("BACKLIGHT_")
            and key not in ("TALOSCONFIG", "TALOS_ENDPOINT", "CONTROL_PLANE_IP")
        }
        self.env.update(
            PATH=str(self.directory) + os.pathsep + os.environ["PATH"], CALL_LOG=str(self.log)
        )

    def command(self, script: str, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(ROOT / script), *args], env=self.env, text=True, capture_output=True, timeout=15
        )

    def calls(self) -> list[list[str]]:
        return (
            [json.loads(line) for line in self.log.read_text().splitlines()]
            if self.log.exists()
            else []
        )

    def test_help_before_build(self) -> None:
        for script in ("backlight", "deploy-backlight"):
            result = self.command(script, "--help")
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), [])

    def test_node_and_inventory_nickname(self) -> None:
        result = self.command(
            "backlight", "--inventory", str(self.inventory), "laptop-a", "percent", "25"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        execution = next(call for call in self.calls() if "exec" in call)
        self.assertIn("pod/pod-a", execution)
        self.assertEqual(execution[-2:], ["percent", "25"])
        self.assertNotIn("--context", execution)

    def test_bulk_continues_after_failure(self) -> None:
        self.env["FAIL_FIRST"] = "1"
        result = self.command("backlight", "all", "off")
        self.assertEqual(result.returncode, 1)
        executions = [call for call in self.calls() if "exec" in call]
        self.assertEqual(len(executions), 2)
        self.assertTrue(any("pod/pod-b" in call for call in executions))
        self.assertFalse(any("pod/pod-old" in call for call in executions))

    def test_invalid_percentage_never_contacts_cluster(self) -> None:
        for value in ("-1", "101", "1.5", "abc"):
            result = self.command("backlight", "all", "percent", value)
            self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), [])

    def test_broad_talos_roles_rejected(self) -> None:
        self.env["MACHINE_CONFIG"] = (
            "apiVersion: v1alpha1\nkind: KubeTalosAPIAccessConfig\n"
            "allowedRoles: [os:admin]\nallowedKubernetesNamespaces: [maintenance]\n"
        )
        result = self.command(
            "deploy-backlight",
            "--endpoint",
            "192.0.2.10",
            "--inventory",
            str(self.inventory),
            "--image",
            IMAGE,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("os:admin", result.stderr)
        self.assertFalse(any("patch" in call for call in self.calls()))

    def test_apply_rejects_mismatched_talos_machine_before_writes(self) -> None:
        self.env["TALOS_MACHINE_ID"] = "b" * 32
        result = self.command(
            "deploy-backlight",
            "--apply",
            "--endpoint",
            "192.0.2.5",
            "--inventory",
            str(self.inventory),
            "--image",
            IMAGE,
            "--kube-context",
            "cluster-a",
            "--talos-context",
            "cluster-b",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match Kubernetes node", result.stderr)
        self.assertFalse(
            any(set(call) & {"patch", "apply", "delete", "pull"} for call in self.calls())
        )

    def test_preview_uses_talosconfig_endpoints_by_default(self) -> None:
        self.env["TALOSCONFIG"] = str(self.directory / "custom-talosconfig")
        result = self.command(
            "deploy-backlight", "--inventory", str(self.inventory), "--image", IMAGE
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [call for call in self.calls() if call[0] == "talosctl"]
        self.assertTrue(calls)
        for call in calls:
            self.assertNotIn("-e", call)
            self.assertEqual(call[call.index("--talosconfig") + 1], self.env["TALOSCONFIG"])

    def test_endpoint_override_precedence(self) -> None:
        self.env.update(CONTROL_PLANE_IP="192.0.2.20", TALOS_ENDPOINT="192.0.2.30")
        for override, expected in (
            ([], "192.0.2.20"),
            (["--endpoint", "192.0.2.40"], "192.0.2.40"),
        ):
            self.log.unlink(missing_ok=True)
            result = self.command(
                "deploy-backlight", "--inventory", str(self.inventory), "--image", IMAGE, *override
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            for call in self.calls():
                if call[0] == "talosctl":
                    self.assertEqual(call[call.index("-e") + 1], expected)

    def test_existing_namespace_requires_explicit_adoption(self) -> None:
        self.env["UNOWNED_NAMESPACE"] = "1"
        args = ("--inventory", str(self.inventory), "--image", IMAGE)
        refused = self.command("deploy-backlight", *args)
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("namespace 'backlight-system' is not owned", refused.stderr)
        self.assertIn("--adopt-namespace", refused.stderr)
        self.assertFalse(any(call[0] == "talosctl" for call in self.calls()))

        preview = self.command("deploy-backlight", *args, "--adopt-namespace")
        self.assertEqual(preview.returncode, 0, preview.stderr)
        self.assertIn("would be adopted with --apply", preview.stderr)
        self.assertNotIn(
            "Namespace", {item["kind"] for item in json.loads(preview.stdout)["items"]}
        )
        self.assertFalse(any(set(call) & {"patch", "apply", "delete"} for call in self.calls()))

    def test_preview_honors_namespace_and_contexts(self) -> None:
        result = self.command(
            "deploy-backlight",
            "--endpoint",
            "192.0.2.10",
            "--inventory",
            str(self.inventory),
            "--image",
            IMAGE,
            "--namespace",
            "panels",
            "--kube-context",
            "example-kube",
            "--talos-context",
            "example-talos",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads(result.stdout)
        self.assertEqual(manifest["items"][0]["metadata"]["name"], "panels")
        daemonset = manifest["items"][3]
        self.assertEqual(daemonset["metadata"]["namespace"], "panels")
        pod = daemonset["spec"]["template"]["spec"]
        (term,) = pod["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ]
        self.assertEqual(term["matchFields"][0]["values"], ["control-plane"])
        self.assertEqual(term["matchExpressions"][0]["key"], "backlight.talos.dev/enrollment")
        secret = next(
            volume["secret"] for volume in pod["volumes"] if volume["name"] == "talos-reader"
        )
        self.assertEqual(secret["items"], [{"key": "config", "path": "talosconfig"}])
        for call in self.calls():
            expected = "example-kube" if call[0] == "kubectl" else "example-talos"
            self.assertIn(expected, call)
            self.assertNotIn("patch", call)
            self.assertNotIn("apply", call)
            self.assertNotIn("delete", call)


if __name__ == "__main__":
    unittest.main()
