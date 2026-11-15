package driver

import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

func TestBuildDevices_PublishesStandardAttributes(t *testing.T) {
	d := &Driver{
		nodeName: "test-node",
		devices:  cxi.MockDevices(1), // "cxi0"
		pending:  make(map[pendingKey]pendingEntry),
	}
	devs := d.buildDevices()
	if len(devs) != 1 {
		t.Fatalf("len(devs) = %d, want 1", len(devs))
	}
	attrs := devs[0].Attributes
	mock := cxi.MockDevices(1)[0]

	numaNode, ok := attrs[resourceapi.QualifiedName("resource.kubernetes.io/numaNode")]
	if !ok {
		t.Fatal("missing resource.kubernetes.io/numaNode attribute")
	}
	if numaNode.IntValue == nil || *numaNode.IntValue != int64(mock.NUMANode) {
		t.Fatalf("numaNode = %+v, want IntValue %d", numaNode, mock.NUMANode)
	}

	pciBusID, ok := attrs[resourceapi.QualifiedName("resource.kubernetes.io/pciBusID")]
	if !ok {
		t.Fatal("missing resource.kubernetes.io/pciBusID attribute")
	}
	if pciBusID.StringValue == nil || *pciBusID.StringValue != mock.PCIBusID {
		t.Fatalf("pciBusID = %+v, want StringValue %q", pciBusID, mock.PCIBusID)
	}

	if _, ok := attrs[resourceapi.QualifiedName("numa_node")]; ok {
		t.Error("legacy numa_node attribute should not be published")
	}
	if _, ok := attrs[resourceapi.QualifiedName("pcie_root")]; ok {
		t.Error("legacy pcie_root attribute should not be published")
	}
	if _, ok := attrs[resourceapi.QualifiedName("resource.kubernetes.io/pcieRoot")]; ok {
		t.Error("GPU-correlation pcieRoot attribute should no longer be published")
	}
}

func TestBuildDevices_OmitsNUMANodeAttributeWhenUnknown(t *testing.T) {
	d := &Driver{
		nodeName: "test-node",
		devices:  cxi.MockDevices(1),
		pending:  make(map[pendingKey]pendingEntry),
	}
	d.devices[0].NUMANode = -1

	devs := d.buildDevices()
	attrs := devs[0].Attributes

	if _, ok := attrs[resourceapi.QualifiedName("resource.kubernetes.io/numaNode")]; ok {
		t.Error("numaNode attribute should be omitted for a device with no NUMA affinity (-1)")
	}
}
