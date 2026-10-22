package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"syscall"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

// EnvName is the kind suffix and device name of the shared env CDI spec,
// which is attached next to every prepared CXI device.
const EnvName = "env"

// EnvHostDir is the container directory the env spec's host libraries are
// mounted under. Keeping injected files out of the image's own lib
// directories makes them visible (ls /host/env) and means we never overwrite
// a file the image ships.
const EnvHostDir = "/host/env"

// legacyProfileSpecs are the profile specs earlier releases wrote. NCCL and
// aws-ofi-nccl are now injected by k8s-modules; WriteEnvSpec removes these
// files so their device IDs stop resolving on nodes upgraded in place.
var legacyProfileSpecs = []string{"nccl", "aws-ofi-nccl"}

// LdcacheConfig controls the createContainer hook that refreshes the
// container's ld.so.cache. cxi-cdi-hook writes the folders to
// /etc/ld.so.conf.d/00-cxi-*.conf, so they are searched ahead of the image's
// own ld.so.conf.d entries. It does not touch LD_LIBRARY_PATH, which the
// loader consults first. The hook and ldconfig host paths are driver flags
// (see CDIEnvConfig.HookPath), not part of this file.
type LdcacheConfig struct {
	// Enabled defaults to true when omitted.
	Enabled *bool `json:"enabled,omitempty"`
}

func (c LdcacheConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

// CDIEnvConfig holds the host runtime the env spec injects. It is loaded from
// a JSON file at driver startup (see LoadConfig) so paths can be updated
// without rebuilding the binary. The zero value (--enable-cdi-env unset)
// yields an env spec with only the side-device nodes and /dev/shm.
type CDIEnvConfig struct {
	Ldcache LdcacheConfig `json:"ldcache,omitzero"`

	// Libs are host files (libfabric, libcxi and their dependencies) mounted
	// read-only at /host/env/<basename>.
	Libs []string `json:"libs"`

	// Env is the list of KEY=value env vars set in every CXI container —
	// notably CXIP_SKIP_AMA_CHECK=1, which makes stock libfabric accept the
	// synthetic HSN netdev whose sysfs the driver overmounts (see
	// internal/netsysfs); without it libfabric's AMA gate (cxip_if.c:580)
	// drops the device.
	Env []string `json:"env,omitempty"`

	// XpmemPath, GdrDrvPath, and KdReg2Path override device-node host paths in
	// tests; when empty the standard /dev/xpmem, /dev/gdrdrv, and /dev/kdreg2
	// are used. kdreg2 is
	// libfabric's kernel-module MR cache monitor (FI_MR_CACHE_MONITOR=kdreg2);
	// exposing /dev/kdreg2 adds the cgroup BPF allowance an unprivileged pod
	// needs to open it, the same way the userfaultfd monitor would otherwise
	// require CAP_SYS_PTRACE on a host with vm.unprivileged_userfaultfd=0.
	XpmemPath  string `json:"-"`
	GdrDrvPath string `json:"-"`
	KdReg2Path string `json:"-"`

	// HookPath overrides the host path of the cxi-cdi-hook binary (see
	// internal/cdi.InstallHook) that the ldcache createContainer hook execs.
	// Empty uses DefaultHookPath.
	HookPath string `json:"-"`

	// LdconfigDir overrides the host directory containing the self-contained
	// ldconfig binary the driver installs at startup (see DefaultLdconfigDir).
	// Empty uses DefaultLdconfigDir.
	LdconfigDir string `json:"-"`
}

// EnvDeviceID returns the CDI device ID of the env spec, e.g.
// "cxi.slingshot.hpe.com/env=env".
func EnvDeviceID(driverName string) string {
	return fmt.Sprintf("%s/%s=%s", driverName, EnvName, EnvName)
}

func profileSpecPath(cdiRoot, driverName, name string) string {
	return filepath.Join(cdiRoot, driverName+"-"+name+".json")
}

// WriteEnvSpec writes the env CDI spec: the xpmem/gdrdrv/kdreg2 device nodes (each
// only when present on the host), /dev/shm, read-only bind mounts of cfg.Libs
// under /host/env, cfg.Env, and the ldcache hook. It also removes the spec
// files of the legacy nccl/aws-ofi-nccl profiles.
func WriteEnvSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}
	for _, name := range legacyProfileSpecs {
		if err := removeSpec(profileSpecPath(cdiRoot, driverName, name)); err != nil {
			return fmt.Errorf("removing legacy CDI %s spec: %w", name, err)
		}
	}

	var deviceNodes []*specs.DeviceNode
	for _, np := range []struct{ override, def string }{
		{cfg.XpmemPath, "/dev/xpmem"},
		{cfg.GdrDrvPath, "/dev/gdrdrv"},
		{cfg.KdReg2Path, "/dev/kdreg2"},
	} {
		hostPath := np.override
		if hostPath == "" {
			hostPath = np.def
		}
		if dn := optionalDeviceNode(hostPath); dn != nil {
			deviceNodes = append(deviceNodes, dn)
		}
	}

	mounts := []*specs.Mount{{
		HostPath:      "/dev/shm",
		ContainerPath: "/dev/shm",
		Options:       []string{"bind", "rw", "nosuid", "nodev", "private"},
	}}
	for _, l := range cfg.Libs {
		mounts = append(mounts, &specs.Mount{
			HostPath:      l,
			ContainerPath: path.Join(EnvHostDir, filepath.Base(l)),
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		})
	}

	var hooks []*specs.Hook
	if cfg.Ldcache.enabled() && len(cfg.Libs) > 0 {
		hooks = append(hooks, ldcacheHook(cfg.HookPath, cfg.LdconfigDir, EnvHostDir))
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/" + EnvName,
		Devices: []specs.Device{{
			Name: EnvName,
			ContainerEdits: specs.ContainerEdits{
				Env:         append([]string(nil), cfg.Env...),
				DeviceNodes: deviceNodes,
				Mounts:      mounts,
				Hooks:       hooks,
			},
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI env spec: %w", err)
	}
	return os.WriteFile(profileSpecPath(cdiRoot, driverName, EnvName), data, 0o644)
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

// ldcacheHook returns a createContainer hook that runs cxi-cdi-hook
// update-ldcache for each provided folder. hookPath is the host path
// cxi-cdi-hook was installed at by InstallHook; empty uses DefaultHookPath.
// ldconfigDir is the host directory containing the self-contained ldconfig
// binary the driver installs at startup; empty uses DefaultLdconfigDir.
func ldcacheHook(hookPath, ldconfigDir string, folders ...string) *specs.Hook {
	if hookPath == "" {
		hookPath = DefaultHookPath
	}
	if ldconfigDir == "" {
		ldconfigDir = DefaultLdconfigDir
	}
	args := []string{hookPath, "update-ldcache", "--ldconfig-dir", ldconfigDir}
	for _, f := range folders {
		args = append(args, "--folder", f)
	}
	return &specs.Hook{
		HookName: "createContainer",
		Path:     hookPath,
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
