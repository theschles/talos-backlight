# Talos backlight

Control laptop display brightness on a Talos Kubernetes cluster. A JSON inventory selects nodes, assigns backlight classes, configures startup brightness, and controls restoration during shutdown or reboot. One DaemonSet serves each hardware class.

```sh
./backlight laptop-a status
./backlight laptop-a percent 25
./backlight all off
./backlight all on
```

## Compatibility

The controller has been tested with Talos v1.14.2 on Linux amd64 using these interfaces.

| Class | Required sysfs interface | Tested hardware |
| --- | --- | --- |
| `apple-gmux` | `/sys/class/backlight/gmux_backlight`, type `platform` | Mid-2010 MacBookPro6,2 |
| `intel-edp` | `/sys/class/backlight/intel_backlight`, type `raw` | Dell Precision 7520 |

The node's kernel must already expose the interface. The MacBook test used a custom Talos kernel with GMUX support. This repository builds the controller image. Kernel customization and Talos installers are separate prerequisites. Other devices exposing the same interfaces need hardware validation.

Run deployment from macOS or Linux with administrator access to Kubernetes and Talos. Controllers run privileged to write the selected sysfs brightness file. Their Talos event-monitoring credential has the `os:reader` role. Deploy them in a dedicated namespace whose access you control.

## Build

Install Python 3.10 or newer, Go 1.26.11 or newer, Make, `kubectl`, and a `talosctl` version compatible with your Talos nodes.

```sh
python3 -m venv .venv
. .venv/bin/activate
python3 -m pip install -r requirements.txt
make test
make build
```

`make build` compiles the current source for Linux amd64 and creates these local files.

- `build/release/controller-amd64.tar` contains the controller image.
- `build/release/image-reference.txt` records its digest-pinned image reference.
- `build/release/SHA256SUMS` records the archive checksum.
- `build/cache/` contains a Talos image cache.

The build uses `gmux.invalid` as a local image-cache registry name. During installation, the script serves the archive from your workstation, temporarily configures a registry mirror on nodes missing the image, pulls the pinned image, and restores the previous mirror configuration. Nodes must be able to reach your workstation on TCP port 3179. Use `--registry-address` and `--registry-port` when the automatically selected address or port is unsuitable.

A source-building Dockerfile is also available.

```sh
docker build --platform linux/amd64 -t talos-backlight:local .
```

The Docker build uses the same source and compiler version. The local Make build produces the archive used by the deployment script.

## Configure

Copy the example and replace its documentation-only IP addresses with your nodes' Kubernetes InternalIP values.

```sh
cp examples/nodes.example.json nodes.json
```

```json
{
  "nodes": [
    {
      "ip": "192.0.2.10",
      "nickname": "laptop-a",
      "startupPercent": 75,
      "restoreOnShutdown": {
        "localShutdown": true,
        "apiShutdown": true,
        "apiReboot": true
      }
    },
    {
      "ip": "192.0.2.11",
      "nickname": "laptop-b",
      "classOverride": "intel-edp"
    }
  ]
}
```

| Field | Behavior |
| --- | --- |
| `ip` | Required. Resolves to one Kubernetes node by InternalIP. |
| `nickname` | Optional unique name accepted by `backlight`. Kubernetes node names also work. |
| `classOverride` | Optional `apple-gmux` or `intel-edp`. Omit to detect the exposed interface. |
| `startupPercent` | Optional integer from 1 to 100, applied once per host boot. Omit to leave startup brightness alone. Saved restoration state takes precedence. |
| `restoreOnShutdown.localShutdown` | Restore for shutdown events without a Talos API actor ID, including a brief power-button press. Defaults to `true`. |
| `restoreOnShutdown.apiShutdown` | Restore when the Talos API begins a shutdown. Defaults to `true`. |
| `restoreOnShutdown.apiReboot` | Restore when the Talos API begins a reboot. Defaults to `true`. |

Local reboot events retain the controller's default restoration behavior. Talos events distinguish shutdown from reboot and API requests from events without an API actor ID. They do not identify whether an API client was `talosctl` or a Kubernetes plugin. Holding the power button to cut power gives the controller no opportunity to restore brightness.

## Preview and deploy

Select your Kubernetes and Talos contexts using the ordinary tool configuration or these optional environment variables.

```sh
export BACKLIGHT_KUBE_CONTEXT=YOUR_KUBERNETES_CONTEXT
export BACKLIGHT_TALOS_CONTEXT=YOUR_TALOS_CONTEXT
export TALOSCONFIG=/path/to/your/talosconfig
```

`kubectl` and `talosctl` use their current contexts when the corresponding overrides are omitted. With no endpoint override, `talosctl` uses the endpoints saved in the selected Talos context. Run `talosctl config info` to check the active context and endpoints. Use `--endpoint` to override them, or export `CONTROL_PLANE_IP` as in the Talos setup guides. The legacy `TALOS_ENDPOINT` variable is also accepted. Precedence is `--endpoint`, `CONTROL_PLANE_IP`, `TALOS_ENDPOINT`, then the Talos context. Use an absolute path for `TALOSCONFIG` so it works from other checkouts. Both commands accept `--inventory`, `--kube-context`, and `--namespace`. Deployment also accepts `--talosconfig` and `--talos-context`.

```sh
./deploy-backlight > preview.json
./deploy-backlight --apply
```

Preview reads the cluster and Talos configuration, detects interfaces, and prints the proposed Kubernetes objects. Apply also performs the following steps.

1. Match the machine ID reported by Kubernetes to the machine ID read from Talos for every control-plane and inventory node. Stop on a missing or mismatched ID. Check for overlapping backlight writers across namespaces. Cache missing images on up to four nodes concurrently and restore each temporary registry setting.
2. Enable Talos-managed API credentials on the control-plane nodes while keeping the role grant limited to `os:reader`.
3. Create the dedicated `backlight-system` namespace and a Talos service account requesting only `os:reader`.
4. Label verified inventory nodes with `backlight-class` and `backlight.talos.dev/enrollment`, then reconcile the class DaemonSets and shared configuration. The enrollment label binds scheduling to the verified node identity. Rerun deployment to enroll a replacement node.
5. Wait for each DaemonSet rollout, then verify manual status, the authenticated event subscription, and restoration readiness on every inventory node.

Talos applies its role allowlist to every permitted namespace. Deployment refuses a cluster-wide grant containing roles beyond `os:reader` and refuses to add that role when doing so would activate grants for other namespaces. Existing namespaces must have `backlight.talos.dev/managed=true` and already enforce the privileged Pod Security profile. For an older installation, verify that the namespace is dedicated to backlight and restrict workload creation there before marking it as managed. Inspect the existing namespace and confirm the Kubernetes context first.

```sh
kubectl config current-context
kubectl get namespace backlight-system --show-labels
kubectl -n backlight-system get daemonsets,deployments,statefulsets,jobs,cronjobs,pods
```

If the namespace is dedicated to backlight, already enforces privileged Pod Security, and only trusted administrators can create workloads there, explicitly approve adoption. Preview reports what would happen and leaves the existing namespace out of the rendered manifest. Apply rechecks eligibility and adds the ownership label before continuing deployment.

```sh
./deploy-backlight --adopt-namespace > preview.json
./deploy-backlight --apply --adopt-namespace
```

If the namespace is shared or enforces a stricter policy, choose a new dedicated namespace with `--namespace`. The same namespace option is available on `./backlight`.

Talos v1.14.2 issues six-hour service account certificates and renews them automatically. The listener reads the latest mounted credential when reconnecting. After a cluster has been off past expiration, renewal occurs as the control plane recovers. Verify connection logs before relying on shutdown restoration.

```sh
kubectl -n backlight-system logs ds/gmux-backlight
kubectl -n backlight-system logs ds/dell-backlight
```

A successful connection logs `Talos shutdown event stream connected`.

Rerun `./deploy-backlight --apply` after editing the inventory or rebuilding. Startup defaults still apply once per host boot. If startup brightness fails, a successful manual brightness command supersedes its retry for that boot. Explicit indefinite `off` state survives routine Pod replacement. Removing a node from a represented class excludes it from that DaemonSet. If the entire class disappears from the inventory, explicitly remove its DaemonSet.

If migrating an earlier installation in another namespace, inspect the generated preview first. `--migrate-from-namespace OLD_NAMESPACE` deletes the represented `gmux-backlight` and `dell-backlight` DaemonSets there, waits for their Pods to stop, and installs their replacements. Include every node served by those DaemonSets in the inventory. Migration checks for active Pods and refuses deletion if a legacy DaemonSet could still schedule on an unlisted node. Existing host restoration files are reused. If apply, rollout, or event verification fails, deployment stops the replacements and attempts to restore the prior DaemonSets, destination ConfigMap, and node labels. Rollback handles each hardware class independently. Before restoring a legacy DaemonSet, it checks that the corresponding destination DaemonSet is absent. If one exists, rollback retains the recovery record for inspection. If a replacement cannot be stopped, the script continues restoring classes whose replacements have stopped. Rollback rechecks that the destination controller is absent before restoring its node labels. It also checks for active Pods left behind by the destination controllers before restoring labels, shared configuration, or legacy DaemonSets.

The deploy script saves private recovery records under `build/recovery/` before changing a temporary mirror or removing legacy workloads. After a crash, interruption, or incomplete rollback, use the record path printed by deployment. Keep these files local because workload snapshots may contain sensitive configuration.

```sh
./deploy-backlight --recover build/recovery/migration-RECORD.json
```

Use the same command with a `mirror-RECORD.json` file to restore a temporary mirror. Migration recovery checks the cluster identity and uses ownership and resource version checks before changing workloads or node labels. An interrupted migration write can still finish on the Kubernetes server after the client exits. Recovery retains the record and pauses rollback when a write was left unacknowledged. Use `--confirm-migration-quiescent` only after verifying that the request can no longer apply. Older records without write tracking also require this confirmation. Records without ownership information require manual recovery. Mirror recovery verifies the original Talos node identity and the temporary mirror configuration before restoring it. Older mirror records without a node identity require manual recovery. A local command still running after interruption keeps the checkout locked. If the mirror still looks original and installation was never confirmed, recovery keeps the record because Talos could still apply the request. Retry recovery after the request settles. Use `--confirm-mirror-quiescent` only after confirming that the interrupted request can no longer apply. A recovered record is removed. Resolve outstanding records before applying again from the same checkout. Recovery covers temporary mirrors and migration workloads. Namespace and Talos credential setup can be reused on retry.

Each controller holds an exclusive lock in its node's shared host state directory. Existing workloads in another namespace must be removed through migration or otherwise excluded from the target nodes before applying. Image cache operations are node-local. Talos access grants, shared Kubernetes objects, and workload transitions are ordered.

## Commands

```sh
./backlight NODE status
./backlight NODE on
./backlight NODE off
./backlight NODE off 30s
./backlight NODE set 2500
./backlight NODE percent 25
./backlight all on
./backlight all off
./backlight all percent 25
```

`NODE` accepts a Kubernetes node name or an inventory nickname. `set` accepts an integer from zero through that panel's reported maximum. `percent` accepts an integer from 0 through 100 and rounds to the nearest device brightness value. Invalid values are rejected before writing. The controller checks the requested and actual values after a write and reports a readback mismatch.

TODO (IPv6 nicknames): Normalize both inventory IPs and Kubernetes InternalIPs during nickname lookup. Until then, use the Kubernetes node name if equivalent IPv6 addresses have different spellings.

An indefinite `off` lasts until `on` or another explicit brightness command. A timed `off` accepts durations from 1 second through 10 minutes and uses the node-local controller timer. Repeated indefinite `off` succeeds and preserves the original restoration value. A positive `set` or `percent` cancels a pending restoration. Setting zero saves the previous lit level when available. `on` restores the saved level, leaves an already-lit panel at its current level, and uses 75% of maximum when the panel is dark with no saved value.

Bulk commands attempt all ready backlight Pods and continue after a node fails. They return a nonzero exit status if any attempted operation fails. Nodes without a ready backlight Pod are excluded. A temporarily unreachable node can still appear ready in Kubernetes until its status updates.

Status JSON reports `requested`, `actual`, `maximum`, and `power`. The `power` field comes from `bl_power`; zero means unblanked. Brightness can be zero while `power` remains zero. Pending restoration adds `restore_at` for timed operations or `off_until_on` for indefinite operations. The `shutdown_listener_connected` boolean reports whether the controller authenticated to the Talos event stream. The separate `shutdown_restoration_ready` boolean also checks panel state, journal recovery, and whether a shutdown sequence is in progress.

If a journal is unreadable or its saved brightness no longer fits the panel, status includes `restoration_error`. Once the panel is unblanked, use `on` to keep a lit panel or recover a dark one to 75% of maximum. You can also use a positive `set` or `percent` to choose the recovery level. A successful recovery clears the invalid journal. Shutdown restoration and dimming remain unavailable until recovery succeeds.

On first enrollment, the controller records a valid lit brightness ratio as the normal level before applying startup settings. A dark panel or an existing enrollment without a recorded normal level uses a 75% fallback. The files live under `/var/lib/gmux-backlight/` on each node. Shutdown restoration raises brightness only when it is below the saved normal level. Ordinary Pod termination does not trigger the shutdown policy. Forceful power loss, a disconnected event listener, or failure before the Pod is available can prevent restoration. If the stream disconnects after shutdown or reboot starts, the controller rejects brightness commands until the matching stop event or two minutes after the disconnect, whichever comes first.

## Probe a node

`./probe-backlight NODE_IP` reports the Talos server version, PCI display devices, bound drivers, installed extensions, and backlight interfaces. It uses the same Talos endpoint and context options as deployment and works independently of the inventory.

Add `--check-extension-catalog` to check the public Image Factory catalog for the exact Talos version when the probe recognizes a documented GPU match. The current match covers Intel UHD Graphics 620 with PCI ID `8086:5917`. A catalog match identifies an extension to investigate. Panel control still requires verification on the node.

## Remove

Restore the panels before removing their controllers.

```sh
./backlight all on
kubectl -n backlight-system delete daemonset gmux-backlight dell-backlight --ignore-not-found
kubectl -n backlight-system delete serviceaccount.talos.dev backlight-talos-reader
kubectl -n backlight-system delete configmap backlight-defaults
```

Remove the `backlight-class` and `backlight.talos.dev/enrollment` labels from the affected nodes. Remove `backlight-system` from the Talos API access namespace allowlist through your Talos configuration workflow, retaining other users' grants. Delete the namespace if it contains only this installation. Host state files persist until explicitly removed, so reinstalling can reuse recorded levels.

## Development and licensing

Repository engineering and review practices are in [AGENTS.md](AGENTS.md). Install the pinned development tools in your activated virtual environment.

```sh
python3 -m pip install -r requirements-dev.txt
```

Run the full quality gate locally before submitting a change.

```sh
make check
```

The gate checks Python formatting and lint with Ruff, Python types with Mypy, Go formatting and vet, and the Go race and Python CLI test suites. Use `make format` to format Python and Go source. Use `make build` to verify the Linux amd64 controller and image packaging when those change. CLI tests simulate cluster responses, so the checks work without a Talos cluster or Docker.

The gate also runs `pip-audit` against runtime and development Python requirements and `govulncheck` against the Linux amd64 Go controller. The Go scan checks dependencies and the standard library for known vulnerabilities reachable from the controller. Run `make audit` to run just these checks. The scanners need network access to resolve dependencies and read current advisory data.

The [GitHub Actions workflow](.github/workflows/checks.yml) runs the same `make check` and `make build` commands on pull requests and pushes to `main`. It uses a standard Ubuntu runner with read-only repository access and uploads no artifacts. Standard GitHub-hosted runners are free for public repositories. [GitHub Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions)

Hardware checks have covered the two devices listed above. The expired-certificate recovery test used a disposable VM with an accelerated guest clock. A new physical installation on other hardware still requires validation.

The project's code is MIT licensed. Dependencies retain their own licenses. See [LICENSE](LICENSE) and [THIRD_PARTY.md](THIRD_PARTY.md). Keep local inventories, credentials, generated previews, and build output out of source control. The repository's example IPs are reserved for documentation.
