// slingshot-cni is the Slingshot CNI plugin binary, invoked locally by
// containerd/Multus as part of CNI ADD/DEL/CHECK. It consumes the CXI DRA
// driver's per-pod hint file (see docs/cnidra-cni-hint-contract.md) to
// resolve the device/hsn_iface/vni a pod was allocated; the actual command
// logic lives in internal/cni, mirroring the cmd/cxi-cdi-hook + internal/cdi
// split already used in this repo for the CDI hook. 
package main

import (
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/slingshot/cxi-dra-driver/internal/cni"
)

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cni.CmdAdd,
		Del:   cni.CmdDel,
		Check: cni.CmdCheck,
	}, version.All, "slingshot-cni")
}
