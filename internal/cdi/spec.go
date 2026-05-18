package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	specs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

const cdiVersion = "0.6.0"

// DeviceID returns the CDI device identifier for a CXI device, e.g.
// "cxi.slingshot.hpe.com/cxi=cxi0".
func DeviceID(driverName, devName string) string {
	return fmt.Sprintf("%s/cxi=%s", driverName, devName)
}

// WriteSpec generates a CDI spec for dev and writes it under cdiRoot.
// DeviceNodes tells containerd to add the device to the container's cgroup
// BPF allowlist at container creation time (before the process starts), which
// is necessary for unprivileged containers to open /dev/cxiN. The sysfs
// bind-mount lets libfabric discover the HSN path.
func WriteSpec(cdiRoot, driverName string, dev *cxi.Device) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	hostDevPath := dev.DevPath
	if dev.HostDevPath != "" {
		hostDevPath = dev.HostDevPath
	}
	containerSysfsPath := "/run/cxi/" + dev.Name

	// Stat the device on the host to get its major/minor numbers.
	// These are needed for the CDI DeviceNode entry so containerd can add the
	// correct cgroup BPF allowance rule.
	var st syscall.Stat_t
	if err := syscall.Stat(hostDevPath, &st); err != nil {
		return fmt.Errorf("stat %s: %w", hostDevPath, err)
	}
	major := int64((uint64(st.Rdev) >> 8) & 0xfff)
	minor := int64((uint64(st.Rdev) & 0xff) | ((uint64(st.Rdev) >> 12) & 0xfff00))

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/cxi",
		Devices: []specs.Device{
			{
				Name: dev.Name,
				ContainerEdits: specs.ContainerEdits{
					// DeviceNodes causes containerd to create the device node in
					// the container and add a cgroup BPF allowance for it — both
					// happen before the container process starts.
					DeviceNodes: []*specs.DeviceNode{
						{
							Path:        dev.DevPath,
							HostPath:    hostDevPath,
							Type:        "c",
							Major:       major,
							Minor:       minor,
							Permissions: "rwm",
						},
					},
					Mounts: []*specs.Mount{
						{
							HostPath:      dev.SysfsPath,
							ContainerPath: containerSysfsPath,
							Options:       []string{"bind", "ro"},
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI spec for %s: %w", dev.Name, err)
	}

	path := filepath.Join(cdiRoot, driverName+"-cxi-"+dev.Name+".json")
	return os.WriteFile(path, data, 0o644)
}

// RemoveSpec deletes the CDI spec file for dev.
func RemoveSpec(cdiRoot, driverName string, dev *cxi.Device) error {
	path := filepath.Join(cdiRoot, driverName+"-cxi-"+dev.Name+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
