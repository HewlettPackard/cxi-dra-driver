# Slingshot CNI Plugin

This document covers the `slingshot-cni` binary: its package layout and its
current hint-consumption behavior. It complements
[docs/cni/cnidra-cni-hint-contract.md](./cnidra-cni-hint-contract.md), which
specifies the driver-side write contract this plugin reads against.

## Binary and package layout

Mirrors the `cmd/cxi-cdi-hook` + `internal/cdi` split already used in this
repo for the CDI hook:

| Path | Role |
|------|------|
| [cmd/slingshot-cni/main.go](../../cmd/slingshot-cni/main.go) | Thin binary entrypoint — wires `internal/cni`'s command functions into `skel.PluginMainFuncs` |
| [internal/cni/](../../internal/cni) | CNI `ADD`/`DEL`/`CHECK` command logic, `CNI_ARGS` parsing, hint-file read/validate |

The binary is built independently of the driver DaemonSet image (`make
build-cni`, CGO-free) — it runs on the host as a CNI plugin invoked by
containerd/Multus, not inside the driver's own container.

## Hint-consumption behavior

On every `ADD`/`DEL`/`CHECK` invocation:

1. Parse the plugin's stdin CNI network configuration (`internal/cni.NetConf`)
   — the standard CNI config header, plus this plugin's own optional
   `hintsDir`, `retryInterval`, and `retryTimeout` overrides.
2. Parse `CNI_ARGS` (passed via `skel.CmdArgs.Args`) for the `K8S_POD_UID`
   key kubelet populates on every invocation — no API-server lookup needed.
3. Read `<hintsDir>/<podUID>.json` (default hints dir:
   `/var/run/cxi/net-hints`, matching the driver's `--cni-hints-dir`
   default), retrying on a bounded interval/timeout (default `100ms`/`5s`)
   if the file isn't there yet. This retry is a safety net for the driver
   crashing between claim allocation and `PrepareResourceClaims` completing
   — see [docs/cnidra-cni-hint-contract.md](../cnidra-cni-hint-contract.md)'s
   ordering guarantee, which this backstops rather than relies on.
4. Validate every resolved hint entry has a non-empty `device`, non-empty
   `hsn_iface`, and a positive `vni`.
5. Log the resolved hint and return success. `ADD` additionally emits a
   minimal CNI result to stdout (required for CNI-spec compliance even
   though no interface exists yet); `DEL`/`CHECK` emit nothing.

Any failure in steps 1–4 (malformed config, missing `K8S_POD_UID`, hint file
never appearing, or an invalid hint entry) fails the command with a
descriptive error.