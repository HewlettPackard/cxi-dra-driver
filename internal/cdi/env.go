package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

// CDIEnvConfig holds configurable paths for environment CDI spec generation.
// XpmemPath and GdrDrvPath default to the standard paths when empty.
type CDIEnvConfig struct {
	LibfabricLib string // host path to libfabric.so.1; empty = skip mount

	NCCLPrefix string // Spack NCCL install dir; empty = skip NCCL spec

	AWSofiNCCLPrefix string // Spack aws-ofi-nccl install dir; empty = skip OFI spec
	HwlocLib         string
	PciaccessLib     string
	Libxml2Lib       string
	LibatomicLib     string
	LiblzmaLib       string
	LibiconvLib      string

	// Optional path overrides for /dev/xpmem and /dev/gdrdrv; used in tests.
	XpmemPath  string
	GdrDrvPath string
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
// nodes, library bind-mounts, and the ldcache hook that every CXI container
// needs. xpmem and gdrdrv device nodes are included only when present on the
// host; a missing libfabric path skips that mount with a warning.
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

	var deviceNodes []*specs.DeviceNode
	for _, p := range []string{xpmemPath, gdrDrvPath} {
		if dn := optionalDeviceNode(p); dn != nil {
			deviceNodes = append(deviceNodes, dn)
		}
	}

	mounts := []*specs.Mount{
		{
			HostPath:      "/dev/shm",
			ContainerPath: "/dev/shm",
			Options:       []string{"bind", "rw", "nosuid", "private"},
		},
	}
	if cfg.LibfabricLib != "" {
		mounts = append(mounts, &specs.Mount{
			HostPath:      cfg.LibfabricLib,
			ContainerPath: "/usr/lib64/libfabric.so.1",
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		})
	}
	for _, so := range []string{
		"/usr/lib64/libcxi.so.1",
		"/usr/lib64/libcurl.so.4",
		"/usr/lib64/libjson-c.so.5",
		"/usr/lib64/libatomic.so.1",
		"/usr/lib64/libnl-3.so.200",
		"/usr/lib64/libnghttp2.so.14",
		"/usr/lib64/libidn2.so.0",
		"/usr/lib64/libssh.so.4",
		"/usr/lib64/libpsl.so.5",
		"/usr/lib64/libssl.so.1.1",
		"/usr/lib64/libcrypto.so.1.1",
		"/usr/lib64/libgssapi_krb5.so.2",
		"/usr/lib64/libldap_r-2.4.so.2",
		"/usr/lib64/liblber-2.4.so.2",
		"/usr/lib64/libzstd.so.1",
		"/usr/lib64/libbrotlidec.so.1",
		"/usr/lib64/libz.so.1",
		"/usr/lib64/libunistring.so.2",
		"/usr/lib64/libjitterentropy.so.3",
		"/usr/lib64/libkrb5.so.3",
		"/usr/lib64/libk5crypto.so.3",
		"/lib64/libcom_err.so.2",
		"/usr/lib64/libkrb5support.so.0",
		"/usr/lib64/libsasl2.so.3",
		"/usr/lib64/libbrotlicommon.so.1",
		"/usr/lib64/libkeyutils.so.1",
		"/usr/lib64/libpcre.so.1",
		"/usr/lib64/libxpmem.so.0",
	} {
		mounts = append(mounts, &specs.Mount{
			HostPath:      so,
			ContainerPath: so,
			Options:       []string{"bind", "ro", "nosuid", "private"},
		})
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/env",
		Devices: []specs.Device{{Name: "env"}},
		ContainerEdits: specs.ContainerEdits{
			DeviceNodes: deviceNodes,
			Mounts:      mounts,
			Hooks:       []*specs.Hook{ldcacheHook("/usr/lib64")},
		},
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

// optionalDeviceNode stats hostPath and returns a DeviceNode if the path
// exists on the host; returns nil without error if the path is absent.
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
		Permissions: "rw",
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
