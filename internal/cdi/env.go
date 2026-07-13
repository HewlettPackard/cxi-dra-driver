package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

// MountSpec is one host→container bind mount in a CDI spec.
type MountSpec struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
}

// CDIEnvConfig holds the paths and env vars that populate the env, NCCL, and
// aws-ofi-nccl CDI specs. It is loaded from a JSON file at driver startup
// (see LoadConfig) so paths can be updated without rebuilding the binary.
type CDIEnvConfig struct {
	// EnvMounts is the library bind-mount list for the env spec. Each entry
	// becomes one ro bind mount. Required when env CDI injection is enabled.
	EnvMounts []MountSpec `json:"envMounts,omitempty"`

	// EnvVars is the list of KEY=value env vars added to the env spec, so every
	// pod that requests the "env" CDI profile receives them. This is the home
	// for settings every libfabric/CXI container needs — notably
	// CXIP_SKIP_AMA_CHECK=1, which makes stock libfabric accept the synthetic
	// HSN netdev whose sysfs the driver overmounts (see internal/netsysfs);
	// without it libfabric's AMA gate (cxip_if.c:580) drops the device.
	EnvVars []string `json:"envVars,omitempty"`

	// NCCLPrefix is the host path bind-mounted to /host/nccl in the NCCL
	// spec. Empty disables the NCCL spec.
	NCCLPrefix string `json:"ncclPrefix,omitempty"`

	// AWSofiMounts is the bind-mount list for the aws-ofi-nccl spec
	// (plugin .so, GDRCopy API, and Spack runtime dependency libs).
	// Empty disables the aws-ofi-nccl spec.
	AWSofiMounts []MountSpec `json:"awsOfiMounts,omitempty"`

	// AWSofiEnv is the list of KEY=value env vars added to the
	// aws-ofi-nccl spec (NCCL_NET, FI_CXI_*, etc).
	AWSofiEnv []string `json:"awsOfiEnv,omitempty"`

	// XpmemPath, GdrDrvPath, and KdReg2Path override device-node host paths in
	// tests; when empty the standard /dev/xpmem, /dev/gdrdrv, and /dev/kdreg2
	// are used. kdreg2 is libfabric's kernel-module MR cache monitor
	// (FI_MR_CACHE_MONITOR=kdreg2); exposing /dev/kdreg2 here — paired with the
	// libfabric mount — adds the cgroup BPF allowance an unprivileged pod needs
	// to open it, the same way the userfaultfd monitor would otherwise require
	// CAP_SYS_PTRACE on a host with vm.unprivileged_userfaultfd=0.
	XpmemPath  string `json:"-"`
	GdrDrvPath string `json:"-"`
	KdReg2Path string `json:"-"`
}

func EnvDeviceID(driverName string) string {
	return fmt.Sprintf("%s/env=env", driverName)
}

func NCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/nccl=nccl", driverName)
}

func AWSofiNCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/aws-ofi-nccl=aws-ofi-nccl", driverName)
}

// WriteEnvSpec writes the shared CXI environment CDI spec containing device
// nodes, library bind-mounts from cfg.EnvMounts, and the ldcache hook that
// every CXI container needs. xpmem and gdrdrv device nodes are included only
// when present on the host.
func WriteEnvSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	xpmemPath := cfg.XpmemPath
	if xpmemPath == "" {
		xpmemPath = "/dev/xpmem"
	}
	gdrDrvPath := cfg.GdrDrvPath
	if gdrDrvPath == "" {
		gdrDrvPath = "/dev/gdrdrv"
	}
	kdreg2Path := cfg.KdReg2Path
	if kdreg2Path == "" {
		kdreg2Path = "/dev/kdreg2"
	}

	var deviceNodes []*specs.DeviceNode
	for _, p := range []string{xpmemPath, gdrDrvPath, kdreg2Path} {
		if dn := optionalDeviceNode(p); dn != nil {
			deviceNodes = append(deviceNodes, dn)
		}
	}

	mounts := []*specs.Mount{
		{
			HostPath:      "/dev/shm",
			ContainerPath: "/dev/shm",
			Options:       []string{"bind", "rw", "nosuid", "nodev", "private"},
		},
	}
	for _, m := range cfg.EnvMounts {
		mounts = append(mounts, &specs.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Options:       []string{"bind", "ro", "nosuid", "private"},
		})
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/env",
		Devices: []specs.Device{{
			Name: "env",
			ContainerEdits: specs.ContainerEdits{
				Env:         append([]string(nil), cfg.EnvVars...),
				DeviceNodes: deviceNodes,
				Mounts:      mounts,
				Hooks:       []*specs.Hook{ldcacheHook("/usr/lib64")},
			},
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI env spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-env.json"), data, 0o644)
}

// RemoveEnvSpec deletes the env CDI spec file.
func RemoveEnvSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-env.json"))
}

// optionalDeviceNode stats hostPath and returns a DeviceNode if the path exists.
// It returns nil if stat fails (including when the path is absent).
func optionalDeviceNode(hostPath string) *specs.DeviceNode {
	var st syscall.Stat_t
	if err := syscall.Stat(hostPath, &st); err != nil {
		return nil
	}
	major := int64((uint64(st.Rdev) >> 8) & 0xfff)
	minor := int64((uint64(st.Rdev) & 0xff) | ((uint64(st.Rdev) >> 12) & 0xfff00))
	return &specs.DeviceNode{
		Path:        hostPath,
		HostPath:    hostPath,
		Type:        "c",
		Major:       major,
		Minor:       minor,
		Permissions: "rwm",
	}
}

// ldcacheHook returns a createContainer hook that runs nvidia-cdi-hook
// update-ldcache for each provided folder.
func ldcacheHook(folders ...string) *specs.Hook {
	args := []string{"nvidia-cdi-hook", "update-ldcache"}
	for _, f := range folders {
		args = append(args, "--folder", f)
	}
	return &specs.Hook{
		HookName: "createContainer",
		Path:     "/usr/bin/nvidia-cdi-hook",
		Args:     args,
	}
}

// removeSpec removes a CDI spec file, treating a missing file as a no-op.
func removeSpec(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WriteNCCLSpec writes the NCCL CDI spec, mounting cfg.NCCLPrefix into
// /host/nccl so the container's ldcache can find the NCCL libraries.
func WriteNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/nccl",
		Devices: []specs.Device{{
			Name: "nccl",
			ContainerEdits: specs.ContainerEdits{
				Mounts: []*specs.Mount{
					{
						HostPath:      cfg.NCCLPrefix,
						ContainerPath: "/host/nccl",
						Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
					},
				},
				Hooks: []*specs.Hook{ldcacheHook("/host/nccl/lib")},
			},
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI NCCL spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-nccl.json"), data, 0o644)
}

// RemoveNCCLSpec deletes the NCCL CDI spec file.
func RemoveNCCLSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-nccl.json"))
}

// WriteAWSofiNCCLSpec writes the AWS OFI NCCL CDI spec, bind-mounting the
// plugin library, GDRCopy API library, and Spack dependency libs from
// cfg.AWSofiMounts, and setting the env vars from cfg.AWSofiEnv.
func WriteAWSofiNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	mounts := make([]*specs.Mount, 0, len(cfg.AWSofiMounts))
	for _, m := range cfg.AWSofiMounts {
		mounts = append(mounts, &specs.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		})
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/aws-ofi-nccl",
		Devices: []specs.Device{{
			Name: "aws-ofi-nccl",
			ContainerEdits: specs.ContainerEdits{
				Env:    append([]string(nil), cfg.AWSofiEnv...),
				Mounts: mounts,
				Hooks:  []*specs.Hook{ldcacheHook("/usr/lib", "/usr/lib64")},
			},
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI aws-ofi-nccl spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-aws-ofi-nccl.json"), data, 0o644)
}

// RemoveAWSofiNCCLSpec deletes the AWS OFI NCCL CDI spec file.
func RemoveAWSofiNCCLSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-aws-ofi-nccl.json"))
}
