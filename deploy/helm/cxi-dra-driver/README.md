# cxi-dra-driver Helm chart

Installs the HPE Slingshot CXI Dynamic Resource Allocation (DRA) driver and its
supporting resources. The **driver name** is parameterized and propagated through
every cluster/API object, so you can install the chart multiple times (e.g. to
A/B two driver versions) and each install is fully isolated.

## What gets installed

| Resource | Name (default driver `cxi.slingshot.hpe.com`) | Notes |
|---|---|---|
| DaemonSet *or* Deployment | `cxi-slingshot-hpe-com` | `workload.kind` selects; pods carry constant `app: cxi-dra-driver` label for anti-affinity |
| ServiceAccount / ClusterRole(+Binding) / Role(+Binding) | `cxi-slingshot-hpe-com*` | Lease `resourceNames` derived from the driver name |
| DeviceClass | `cxi-slingshot-hpe-com-device` | CEL selects this driver's ResourceSlices |
| MutatingAdmissionPolicy + Binding | `cxi-slingshot-hpe-com-capacity-conversion` | converts legacy opaque resource config on claims for this driver into capacity requests |
| ConfigMap | `cxi-slingshot-hpe-com-env` | `env.json` runtime-library mounts |
| ResourceClaimTemplates | `cxi-slingshot-hpe-com-{small,medium,large,whole,2-small,2-whole}` | one set per `resourceClaimTemplates.namespaces` entry; point at this install's DeviceClass |
| NetworkAttachmentDefinition (opt-in) | `slingshot-hsn` | only when `cni.nad.create=true`; one per `cni.nad.namespaces` (falls back to the claim-template namespaces); requires Multus + Whereabouts |
| MutatingAdmissionPolicy + Binding (opt-in) | `cxi-slingshot-hpe-com-slingshot-network-injection` | only when `cni.nad.create=true`; adds the Multus `k8s.v1.cni.cncf.io/networks` annotation to pods that reference this install's claim templates |

## Isolation model

- **Unique per install:** every named object above keys off the (sanitized) driver
  name, so two installs never collide on cluster/API objects.
- **Constant across installs:** the pod label `app: cxi-dra-driver`
  (`workload.antiAffinity.label`). The pod anti-affinity matches *this* label, so
  pods from different installs repel each other and land on **different nodes** —
  exactly what you want for side-by-side version testing.
- **Fixed, version-independent node paths:** `--plugin-dir` and the containerd CDI
  drop-in path are NOT driver-name-derived. Anti-affinity already guarantees one
  driver pod per node, and stable paths let `helm upgrade` cleanly replace the
  previous version's node resources instead of orphaning them.

## Install

```bash
# Default install (driver name cxi.slingshot.hpe.com, DaemonSet, kube-system)
helm install cxi deploy/helm/cxi-dra-driver

# A second install under a different driver name for version testing
helm install cxi-next deploy/helm/cxi-dra-driver \
  --set driverName=cxi.next.hpe.com \
  --set image.tag=next

# Deployment instead of DaemonSet, with N replicas spread across nodes
helm install cxi deploy/helm/cxi-dra-driver \
  --set workload.kind=Deployment --set workload.replicas=10
```

## Key values

See [values.yaml](values.yaml) for the full set. Most-used:

| Value | Default | Purpose |
|---|---|---|
| `driverName` | `cxi.slingshot.hpe.com` | DRA driver name; drives all object names |
| `workload.kind` | `DaemonSet` | `DaemonSet` or `Deployment` |
| `workload.replicas` | `10` | Deployment replica count |
| `workload.antiAffinity.enabled` | `true` | spread pods onto distinct nodes |
| `image.repository` / `image.tag` | `ghcr.io/bristol-supercomputing/cxi-dra` / `main` | driver image; every build is also tagged with its commit sha, pin one for a reproducible install |
| `namespace` | `kube-system` | driver components namespace |
| `priorityClassName` | `system-node-critical` | set `""` if installing outside kube-system |
| `driver.vniRange.start/end` | `100`/`300` | per-namespace VNI pool |
| `driver.systemSvcIDs` | `[1,2,3,4]` | blessed system service svc_ids (never reaped; their usage is reserved) |
| `resourceClaimTemplates.namespaces` | `[default]` | namespaces the templates are rendered into, one copy each (falls back to singular `namespace` when empty) |
| `admissionPolicy.excludedNamespaces` | `[kube-system]` | namespaces the policy ignores |
| `cni.install` | `false` | install the bundled `slingshot-cni` into `cni.binDir` (default `/opt/cni/bin`) at driver startup |
| `cni.nad.create` | `false` | render the `slingshot-hsn` NetworkAttachmentDefinition and the pod network-injection policy; `cni.nad.ipam` is the Whereabouts block |

## Verify a render

```bash
helm lint deploy/helm/cxi-dra-driver
helm template t deploy/helm/cxi-dra-driver | kubectl apply --dry-run=client -f -
```
