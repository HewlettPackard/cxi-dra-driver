package cxi

import (
	"os"
	"path/filepath"
	"strings"
)

const sysfsPCIDevicesPath = "/sys/bus/pci/devices"

// nvidiaVendorID is the PCI vendor ID for NVIDIA, as written in
// /sys/bus/pci/devices/<bdf>/vendor.
const nvidiaVendorID = "0x10de"

// GPUPCIeRootByNUMA scans sysfs for NVIDIA GPUs and returns a map from NUMA
// node to the GPU's PCIe root (e.g. "pci0009:00").
//
// This mirrors the value the NVIDIA DRA driver publishes as the standardized
// resource.kubernetes.io/pcieRoot device attribute. The CXI driver republishes
// the NUMA-local GPU's root on each CXI device so a single ResourceClaim can
// use matchAttribute on resource.kubernetes.io/pcieRoot to co-locate a CXI NIC
// with its NUMA-local GPU — NVIDIA does not publish numa_node, and the NIC and
// GPU sit under different PCIe roots, so NUMA is the only shared locality key.
//
// Returns an empty (non-nil) map if no NVIDIA GPUs are present.
func GPUPCIeRootByNUMA() map[int]string {
	out := make(map[int]string)

	entries, err := os.ReadDir(sysfsPCIDevicesPath)
	if err != nil {
		return out
	}

	for _, e := range entries {
		bdf := e.Name()
		devPath := filepath.Join(sysfsPCIDevicesPath, bdf)

		if !isNVIDIAGPU(devPath) {
			continue
		}

		numa := readIntFile(filepath.Join(devPath, "numa_node"), -1)
		if numa < 0 {
			continue
		}

		root := pcieRootFromDevice(devPath)
		if root == "" {
			continue
		}

		// First GPU wins for a given NUMA node. On GH200 the mapping is 1:1,
		// so collisions are not expected.
		if _, ok := out[numa]; !ok {
			out[numa] = root
		}
	}

	return out
}

// isNVIDIAGPU reports whether the PCI device at devPath is an NVIDIA display
// controller (vendor 0x10de, PCI class 0x03xxxx).
func isNVIDIAGPU(devPath string) bool {
	vendor := strings.TrimSpace(readStringFile(filepath.Join(devPath, "vendor")))
	if vendor != nvidiaVendorID {
		return false
	}
	class := strings.TrimSpace(readStringFile(filepath.Join(devPath, "class")))
	// Class is "0xCCSSPP"; the high byte 0x03 is the display-controller class
	// (VGA, 3D controller, etc.).
	return strings.HasPrefix(class, "0x03")
}

// pcieRootFromDevice resolves the sysfs symlink for a PCI device and returns
// the PCIe root component of its physical path, e.g. "pci0009:00" for a device
// under /sys/devices/pci0009:00/0009:01:00.0.
func pcieRootFromDevice(devPath string) string {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return ""
	}
	for part := range strings.SplitSeq(real, string(os.PathSeparator)) {
		if strings.HasPrefix(part, "pci") {
			return part
		}
	}
	return ""
}

// readStringFile returns the contents of path, or "" on error.
func readStringFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
