"""Exercise rollout failure, credential verification, and registry rollback."""

import json
import runpy
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

import yaml

ROOT = Path(__file__).resolve().parents[1]


class DeploymentTest(unittest.TestCase):
    """Use a deployment command fixture without contacting live nodes."""

    def setUp(self) -> None:
        self.module = runpy.run_path(str(ROOT / "deploy-backlight"))
        self.scope = self.module["plan"].__globals__
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.scope["ROOT"] = Path(temporary.name)
        self.daemonset = yaml.safe_load((ROOT / "templates/apple-gmux.yaml").read_text())

    def test_unmanaged_namespace_is_refused(self) -> None:
        with (
            patch.dict(
                self.scope,
                {
                    "get_object": Mock(
                        return_value={
                            "metadata": {
                                "labels": {
                                    "pod-security.kubernetes.io/enforce": "restricted",
                                }
                            }
                        }
                    )
                },
            ),
            self.assertRaisesRegex(ValueError, "not owned.*--adopt-namespace"),
        ):
            self.module["inspect_namespace"](["kubectl"], "shared", adopt=False)

    def test_adoption_checks_workloads_and_patches_only_ownership(self) -> None:
        namespace = {
            "metadata": {
                "name": "backlight-system",
                "uid": "fixture",
                "resourceVersion": "42",
                "labels": {"pod-security.kubernetes.io/enforce": "privileged"},
            }
        }
        workloads = [
            {"kind": "DaemonSet", "metadata": {"name": "gmux-backlight", "uid": "gmux"}},
            {
                "kind": "Pod",
                "metadata": {
                    "name": "panel",
                    "ownerReferences": [
                        {"kind": "DaemonSet", "uid": "gmux", "controller": True},
                    ],
                },
            },
        ]
        for unrelated in (False, True):
            with self.subTest(unrelated=unrelated):
                items = workloads + (
                    [{"kind": "Job", "metadata": {"name": "other"}}] if unrelated else []
                )
                command = Mock(return_value=json.dumps({"items": items}))
                with patch.dict(
                    self.scope, {"get_object": Mock(return_value=namespace), "run": command}
                ):
                    if unrelated:
                        with self.assertRaisesRegex(ValueError, "unrelated Job"):
                            self.module["reconcile_namespace"](
                                ["kubectl"], namespace, namespace, adopt=True
                            )
                        self.assertEqual(command.call_count, 1)
                    else:
                        self.module["reconcile_namespace"](
                            ["kubectl"], namespace, namespace, adopt=True
                        )
                        self.assertEqual(command.call_count, 2)
                        self.assertEqual(
                            json.loads(command.call_args.args[0][-1]),
                            {
                                "metadata": {
                                    "uid": "fixture",
                                    "resourceVersion": "42",
                                    "labels": {"backlight.talos.dev/managed": "true"},
                                }
                            },
                        )

    def test_mirror_replacement_deletes_before_restoring(self) -> None:
        previous = {
            "apiVersion": "v1alpha1",
            "kind": "RegistryMirrorConfig",
            "name": "gmux.invalid",
            "endpoints": [{"url": "http://192.0.2.20:5000"}],
        }
        patch_command = Mock(return_value="")
        with patch.dict(
            self.scope, {"run": patch_command, "registry_mirror": Mock(return_value=previous)}
        ):
            self.module["replace_mirror"](["talosctl"], previous)
        deletion, restored = yaml.safe_load_all(patch_command.call_args.args[0][-1])
        self.assertEqual(deletion["$patch"], "delete")
        self.assertEqual(restored, previous)

    def verified_node(self, uid: str = "fixture-host") -> dict[str, object]:
        """Build a Kubernetes node snapshot for workload deployment."""
        return {
            "metadata": {"name": "host", "uid": uid, "resourceVersion": "10", "labels": {}},
            "status": {"nodeInfo": {"machineID": "a" * 32}},
        }

    def test_apply_refuses_replacement_node(self) -> None:
        command = Mock()
        with (
            patch.dict(
                self.scope,
                {
                    "get_object": Mock(return_value=self.verified_node("replacement")),
                    "run": command,
                },
            ),
            self.assertRaisesRegex(ValueError, "node host was replaced"),
        ):
            self.module["apply_workloads"](
                ["kubectl"],
                "new",
                None,
                {},
                [self.daemonset],
                {"host": "apple-gmux"},
                {"host": self.verified_node()},
            )
        command.assert_not_called()

    def test_apply_sets_enrollment_with_node_preconditions(self) -> None:
        snapshot = self.verified_node()
        enrollment = self.module["enrollment_id"](snapshot)
        command = Mock(return_value="")
        with patch.dict(
            self.scope,
            {
                "get_object": Mock(return_value=snapshot),
                "run": command,
                "wait_rollout": Mock(),
                "verify_controllers": Mock(),
            },
        ):
            self.module["apply_workloads"](
                ["kubectl"],
                "new",
                None,
                {},
                [self.daemonset],
                {"host": "apple-gmux"},
                {"host": snapshot},
            )
        args = command.call_args_list[0].args[0]
        metadata = json.loads(args[-1])["metadata"]
        self.assertEqual(metadata["uid"], "fixture-host")
        self.assertEqual(metadata["resourceVersion"], "10")
        self.assertEqual(metadata["labels"]["backlight.talos.dev/enrollment"], enrollment)

    def test_rollout_failure_restores_legacy_workload(self) -> None:
        restore = Mock(return_value=None)
        rollout = Mock(side_effect=[ValueError("image pull failed"), None])
        run = Mock(return_value='{"items": []}')
        label = Mock()
        old = {"metadata": {"name": "gmux-backlight", "namespace": "old"}}
        cluster = {"metadata": {"uid": "fixture-cluster"}}
        original = Mock(
            side_effect=[
                None,
                old,
                None,
                self.verified_node(),
                cluster,
                cluster,
                None,
                None,
                None,
                None,
            ]
        )
        with (
            patch.dict(
                self.scope,
                {
                    "run": run,
                    "wait_rollout": rollout,
                    "restore_object": restore,
                    "check_overlaps": Mock(),
                    "migration_label": label,
                    "get_object": original,
                },
            ),
            self.assertRaisesRegex(ValueError, "image pull failed"),
        ):
            self.module["apply_workloads"](
                ["kubectl"],
                "new",
                "old",
                {"kind": "ConfigMap", "metadata": {"name": "defaults"}},
                [self.daemonset],
                {"host": "apple-gmux"},
                {"host": self.verified_node()},
            )
        self.assertEqual(restore.call_args_list[0].args[1:], ("ds", "gmux-backlight", "old", None))
        self.assertEqual(restore.call_args_list[3].args[1:], ("ds", "gmux-backlight", "new", None))
        self.assertEqual(restore.call_args_list[-1].args[1:], ("ds", "gmux-backlight", "old", old))
        self.assertTrue(label.call_args.kwargs["rollback"])

    def test_unacknowledged_create_blocks_rollback(self) -> None:
        restore = Mock(
            side_effect=[None, None, subprocess.TimeoutExpired("create", 45), None, None, None]
        )
        run = Mock()
        old = {"metadata": {"name": "gmux-backlight", "namespace": "old"}}
        cluster = {"metadata": {"uid": "fixture-cluster"}}
        original = Mock(side_effect=[None, old, None, self.verified_node(), cluster, cluster, None])
        with (
            patch.dict(
                self.scope,
                {
                    "run": run,
                    "wait_rollout": Mock(),
                    "restore_object": restore,
                    "check_overlaps": Mock(),
                    "migration_label": Mock(),
                    "get_object": original,
                },
            ),
            self.assertRaisesRegex(ValueError, "install new/ds/gmux-backlight"),
        ):
            self.module["apply_workloads"](
                ["kubectl"],
                "new",
                "old",
                {"kind": "ConfigMap", "metadata": {"name": "defaults"}},
                [self.daemonset],
                {"host": "apple-gmux"},
                {"host": self.verified_node()},
            )
        self.assertEqual(restore.call_count, 3)
        (journal,) = (self.scope["ROOT"] / "build/recovery").glob("*.json")
        self.assertEqual(
            json.loads(journal.read_text())["in_flight"], "install new/ds/gmux-backlight"
        )

    def test_manual_status_does_not_imply_shutdown_readiness(self) -> None:
        pods = {
            "items": [
                {
                    "metadata": {"name": "pod", "ownerReferences": [{"uid": "controller"}]},
                    "spec": {"nodeName": "host"},
                    "status": {"phase": "Running"},
                }
            ]
        }
        for connected in (False, True):
            with (
                self.subTest(connected=connected),
                patch.dict(
                    self.scope,
                    {
                        "get_object": Mock(return_value={"metadata": {"uid": "controller"}}),
                        "run": Mock(
                            side_effect=[
                                json.dumps(pods),
                                json.dumps(
                                    {
                                        "requested": 50,
                                        "shutdown_listener_connected": connected,
                                        "shutdown_restoration_ready": connected,
                                    }
                                ),
                            ]
                        ),
                        "time": SimpleNamespace(monotonic=Mock(side_effect=[0, 31])),
                    },
                ),
            ):
                args = (["kubectl"], "new", [self.daemonset], {"host": "apple-gmux"})
                if connected:
                    self.module["verify_controllers"](*args)
                else:
                    with self.assertRaisesRegex(ValueError, "restoration unavailable on host"):
                        self.module["verify_controllers"](*args)

    def test_connected_listener_with_bad_journal_fails_verification(self) -> None:
        pod = {
            "metadata": {"name": "pod", "ownerReferences": [{"uid": "controller"}]},
            "spec": {"nodeName": "host"},
            "status": {"phase": "Running"},
        }
        with (
            patch.dict(
                self.scope,
                {
                    "get_object": Mock(return_value={"metadata": {"uid": "controller"}}),
                    "run": Mock(
                        side_effect=[
                            json.dumps({"items": [pod]}),
                            json.dumps(
                                {
                                    "shutdown_listener_connected": True,
                                    "shutdown_restoration_ready": False,
                                    "restoration_error": "invalid journal",
                                }
                            ),
                        ]
                    ),
                    "time": SimpleNamespace(monotonic=Mock(side_effect=[0, 31])),
                },
            ),
            self.assertRaisesRegex(ValueError, "restoration unavailable on host"),
        ):
            self.module["verify_controllers"](
                ["kubectl"], "new", [self.daemonset], {"host": "apple-gmux"}
            )

    def test_interrupted_migration_can_be_recovered_from_disk(self) -> None:
        old = {"metadata": {"name": "gmux-backlight", "namespace": "old"}}
        cluster = {"metadata": {"uid": "fixture-cluster"}}
        original = Mock(side_effect=[None, old, None, self.verified_node(), cluster, cluster, None])
        restore = Mock(side_effect=[SystemExit(143), None])
        with patch.dict(
            self.scope,
            {
                "get_object": original,
                "check_overlaps": Mock(),
                "run": Mock(return_value='{"items": []}'),
                "restore_object": restore,
                "wait_rollout": Mock(),
            },
        ):
            with self.assertRaises(SystemExit):
                self.module["apply_workloads"](
                    ["kubectl"],
                    "new",
                    "old",
                    {"kind": "ConfigMap", "metadata": {"name": "defaults"}},
                    [self.daemonset],
                    {"host": "apple-gmux"},
                    {"host": self.verified_node()},
                )
            (journal,) = (self.scope["ROOT"] / "build/recovery").glob("*.json")
            self.assertEqual(journal.stat().st_mode & 0o777, 0o600)
            with self.assertRaisesRegex(ValueError, "may still be in flight"):
                self.module["recover_record"](journal, json.loads(journal.read_text()))
            self.assertTrue(journal.exists())
            self.module["recover_record"](
                journal, json.loads(journal.read_text()), confirm_migration_quiescent=True
            )
            self.assertFalse(journal.exists())
        self.assertEqual(restore.call_args_list[-1].args[1:], ("ds", "gmux-backlight", "old", old))

    def test_rollback_recovers_peer_when_one_replacement_cannot_stop(self) -> None:
        old_gmux = {"metadata": {"name": "gmux-backlight", "namespace": "old"}}
        old_dell = {"metadata": {"name": "dell-backlight", "namespace": "old"}}
        record = {
            "kind": "migration",
            "kube": ["kubectl"],
            "cluster_uid": "fixture-cluster",
            "namespace": "new",
            "legacy": "old",
            "names": ["gmux-backlight", "dell-backlight"],
            "snapshots": [
                ("ds", "gmux-backlight", "old", old_gmux),
                ("ds", "dell-backlight", "old", old_dell),
            ],
            "removed": ["gmux-backlight", "dell-backlight"],
            "relabeled": [],
            "token": "fixture-token",
            "attempted": ["gmux-backlight", "dell-backlight"],
            "in_flight": None,
            "stopped": [],
            "labels_restored": [],
            "objects_restored": [],
        }
        journal = self.module["begin_recovery"](record)
        restore = Mock(side_effect=[OSError("cannot stop GMUX"), None, None])
        with patch.dict(
            self.scope,
            {
                "get_object": Mock(
                    side_effect=lambda _, kind, *args: (
                        {"metadata": {"uid": "fixture-cluster"}} if kind == "namespace" else None
                    )
                ),
                "run": Mock(return_value='{"items": []}'),
                "restore_object": restore,
                "wait_rollout": Mock(),
            },
        ):
            with self.assertRaisesRegex(ValueError, "rollback incomplete"):
                self.module["recover_record"](journal, record)
            self.assertEqual(
                restore.call_args_list[-1].args[1:], ("ds", "dell-backlight", "old", old_dell)
            )
            restore.reset_mock(side_effect=True)
            self.module["recover_record"](journal, json.loads(journal.read_text()))
        self.assertEqual([call.args[2] for call in restore.call_args_list], ["gmux-backlight"] * 2)
        self.assertFalse(journal.exists())

    def test_mirror_recovery_refuses_unrelated_changes(self) -> None:
        previous = {"endpoints": [{"url": "http://192.0.2.20:5000"}]}
        temporary = {"endpoints": [{"url": "http://192.0.2.30:3179"}]}
        record = {
            "kind": "mirror",
            "node_id": "original",
            "talos": ["talosctl", "-n", "192.0.2.10"],
            "previous": previous,
            "temporary": temporary,
        }
        journal = self.module["begin_recovery"](record)
        replace = Mock()
        with patch.dict(
            self.scope,
            {
                "talos_node_id": Mock(return_value="original"),
                "registry_mirror": Mock(side_effect=[{"endpoints": []}, temporary]),
                "replace_mirror": replace,
            },
        ):
            with self.assertRaisesRegex(ValueError, "changed externally"):
                self.module["recover_record"](journal, json.loads(journal.read_text()))
            replace.assert_not_called()
            self.module["recover_record"](journal, json.loads(journal.read_text()))
        replace.assert_called_once_with(record["talos"], previous)
        self.assertFalse(journal.exists())

    def test_overlap_in_another_namespace_is_refused(self) -> None:
        self.daemonset["metadata"]["namespace"] = "other"
        run = Mock(return_value=json.dumps({"items": [self.daemonset]}))
        with (
            patch.dict(self.scope, {"run": run}),
            self.assertRaisesRegex(ValueError, "other/gmux-backlight"),
        ):
            self.module["check_overlaps"](
                ["kubectl"], "new", None, [self.daemonset], {"host": "apple-gmux"}
            )

    def test_unconfirmed_mirror_keeps_recovery_record(self) -> None:
        record: dict[str, object] = {
            "kind": "mirror",
            "node_id": "original",
            "talos": ["talosctl"],
            "previous": None,
            "temporary": {"endpoints": []},
            "install_confirmed": False,
        }
        journal = self.module["begin_recovery"](record)
        with (
            patch.dict(
                self.scope,
                {
                    "registry_mirror": Mock(return_value=None),
                    "talos_node_id": Mock(return_value="original"),
                },
            ),
            self.assertRaisesRegex(ValueError, "in flight"),
        ):
            self.module["recover_record"](journal, record)
        self.assertTrue(journal.exists())
        with patch.dict(
            self.scope,
            {
                "registry_mirror": Mock(return_value=None),
                "talos_node_id": Mock(return_value="original"),
            },
        ):
            self.module["recover_record"](journal, record, confirm_quiescent=True)
        self.assertFalse(journal.exists())

    def test_migration_rejects_unlisted_legacy_pod(self) -> None:
        self.daemonset["metadata"]["namespace"] = "old"
        pod = {
            "metadata": {
                "name": "legacy",
                "namespace": "old",
                "ownerReferences": [
                    {"name": "gmux-backlight", "kind": "DaemonSet", "controller": True},
                ],
            },
            "spec": {"nodeName": "unlisted"},
            "status": {"phase": "Running"},
        }
        run = Mock(
            side_effect=[
                json.dumps({"items": []}),
                json.dumps({"items": [self.daemonset]}),
                json.dumps({"items": [pod]}),
            ]
        )
        with (
            patch.dict(self.scope, {"run": run}),
            self.assertRaisesRegex(ValueError, "unlisted node unlisted"),
        ):
            self.module["check_overlaps"](
                ["kubectl"], "new", "old", [self.daemonset], {"host": "apple-gmux"}
            )

    def test_stale_recovery_cannot_delete_new_workload(self) -> None:
        newer = {"metadata": {"uid": "newer", "resourceVersion": "10"}}
        command = Mock()
        with (
            patch.dict(self.scope, {"get_object": Mock(return_value=newer), "run": command}),
            self.assertRaisesRegex(ValueError, "changed outside"),
        ):
            self.module["restore_object"](
                ["kubectl"], "ds", "gmux-backlight", "new", None, owner="migration-token"
            )
        command.assert_not_called()

    def test_migration_rejects_unlisted_eligible_node_without_pod(self) -> None:
        self.daemonset["metadata"]["namespace"] = "old"
        unlisted = {"metadata": {"name": "other", "labels": {"backlight-class": "apple-gmux"}}}
        run = Mock(
            side_effect=[json.dumps({"items": [unlisted]}), json.dumps({"items": [self.daemonset]})]
        )
        with (
            patch.dict(self.scope, {"run": run}),
            self.assertRaisesRegex(ValueError, "may serve unlisted node other"),
        ):
            self.module["check_overlaps"](
                ["kubectl"], "new", "old", [self.daemonset], {"host": "apple-gmux"}
            )

    def test_recovery_delete_uses_uid_and_resource_version(self) -> None:
        current = {
            "metadata": {
                "uid": "replacement",
                "resourceVersion": "10",
                "annotations": {"backlight.talos.dev/migration": "fixture-token"},
            }
        }
        command = Mock()
        with patch.dict(self.scope, {"get_object": Mock(return_value=current), "run": command}):
            self.module["restore_object"](
                ["kubectl"], "ds", "gmux-backlight", "new", None, owner="fixture-token"
            )
        args, payload = command.call_args_list[0].args
        self.assertEqual(args[1:3], ["delete", "--raw"])
        self.assertEqual(
            json.loads(payload)["preconditions"], {"uid": "replacement", "resourceVersion": "10"}
        )

    def test_recovery_refuses_new_revision_with_old_token(self) -> None:
        newer = {
            "metadata": {
                "uid": "replacement",
                "generation": 2,
                "resourceVersion": "10",
                "annotations": {"backlight.talos.dev/migration": "fixture-token"},
            }
        }
        command = Mock()
        with (
            patch.dict(self.scope, {"get_object": Mock(return_value=newer), "run": command}),
            self.assertRaisesRegex(ValueError, "changed outside"),
        ):
            self.module["restore_object"](
                ["kubectl"],
                "ds",
                "gmux-backlight",
                "new",
                None,
                owner="fixture-token",
                identity={"uid": "replacement", "generation": 1},
            )
        command.assert_not_called()

    def test_rollback_retry_refuses_recreated_destination(self) -> None:
        name = "gmux-backlight"
        old = {"metadata": {"name": name, "namespace": "old"}}
        newer = {"metadata": {"uid": "new-controller", "name": name, "namespace": "new"}}
        record: dict[str, object] = {
            "kind": "migration",
            "kube": ["kubectl"],
            "cluster_uid": "fixture",
            "namespace": "new",
            "legacy": "old",
            "names": [name],
            "attempted": [name, "backlight-defaults"],
            "stopped": [name],
            "removed": [name],
            "relabeled": ["host"],
            "labels_restored": [],
            "node_controllers": {"host": name},
            "previous_nodes": {"host": {}},
            "desired_labels": {"host": "apple-gmux"},
            "objects_restored": [],
            "snapshots": [
                ("configmap", "backlight-defaults", "new", None),
                ("ds", name, "old", old),
            ],
            "in_flight": None,
            "token": "fixture",
        }
        journal = self.module["begin_recovery"](record)
        restore = Mock()
        label = Mock()
        get = Mock(
            side_effect=lambda _, kind, *args: (
                {"metadata": {"uid": "fixture"}} if kind == "namespace" else newer
            )
        )
        with (
            patch.dict(
                self.scope,
                {
                    "get_object": get,
                    "restore_object": restore,
                    "wait_rollout": Mock(),
                    "migration_label": label,
                },
            ),
            self.assertRaisesRegex(ValueError, "destination.*still exists"),
        ):
            self.module["recover_record"](journal, record)
        self.assertTrue(journal.exists())
        restore.assert_not_called()
        label.assert_not_called()

    def test_rollback_refuses_orphan_controller_pod(self) -> None:
        name = "gmux-backlight"
        old = {"metadata": {"name": name, "namespace": "old"}}
        pod = {
            "metadata": {"name": "orphan", "labels": {"app.kubernetes.io/name": name}},
            "spec": {"volumes": [{"configMap": {"name": "backlight-defaults"}}]},
            "status": {"phase": "Running"},
        }
        record: dict[str, object] = {
            "kind": "migration",
            "kube": ["kubectl"],
            "cluster_uid": "fixture",
            "namespace": "new",
            "legacy": "old",
            "names": [name],
            "attempted": [name, "backlight-defaults"],
            "stopped": [name],
            "removed": [name],
            "relabeled": ["host"],
            "labels_restored": [],
            "node_controllers": {"host": name},
            "previous_nodes": {"host": {}},
            "desired_labels": {"host": "apple-gmux"},
            "objects_restored": [],
            "snapshots": [
                ("configmap", "backlight-defaults", "new", None),
                ("ds", name, "old", old),
            ],
            "in_flight": None,
            "token": "fixture",
        }
        journal = self.module["begin_recovery"](record)
        restore = Mock()
        label = Mock()
        get = Mock(
            side_effect=lambda _, kind, *args: (
                {"metadata": {"uid": "fixture"}} if kind == "namespace" else None
            )
        )
        with (
            patch.dict(
                self.scope,
                {
                    "get_object": get,
                    "run": Mock(return_value=json.dumps({"items": [pod]})),
                    "restore_object": restore,
                    "wait_rollout": Mock(),
                    "migration_label": label,
                },
            ),
            self.assertRaisesRegex(ValueError, "active destination Pod orphan"),
        ):
            self.module["recover_record"](journal, record)
        self.assertTrue(journal.exists())
        restore.assert_not_called()
        label.assert_not_called()

    def test_mirror_recovery_refuses_reused_address(self) -> None:
        temporary: dict[str, object] = {"endpoints": []}
        record = {
            "kind": "mirror",
            "talos": ["talosctl"],
            "node_id": "original",
            "previous": None,
            "temporary": temporary,
            "install_confirmed": True,
        }
        journal = self.module["begin_recovery"](record)
        replace = Mock()
        with (
            patch.dict(
                self.scope,
                {
                    "run": Mock(return_value=json.dumps({"spec": {"nodeId": "different"}})),
                    "registry_mirror": Mock(return_value=temporary),
                    "replace_mirror": replace,
                },
            ),
            self.assertRaisesRegex(ValueError, "different Talos node"),
        ):
            self.module["recover_record"](journal, record)
        self.assertTrue(journal.exists())
        replace.assert_not_called()
