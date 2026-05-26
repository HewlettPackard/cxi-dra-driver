package cxi

// ResourceLimits holds the maximum and reserved counts for a single CXI
// hardware resource type on one device.
type ResourceLimits struct {
	Max int64
	Res int64
}

// ResourceSet describes the full set of allocatable hardware resources on a
// single CXI device. Field names match the cxi_svc_desc resource names used
// by libcxi and the cxi_service CLI.
//
// On real hardware these values are read from the device at discovery time.
// In mock mode, DefaultResourceSet() provides sensible defaults derived from
// the Slurm HPE Slingshot switch plugin (switch_hpe_slingshot.h).
type ResourceSet struct {
	TXQs ResourceLimits // Transmit Queues
	TGQs ResourceLimits // Target (Get) Queues
	EQs  ResourceLimits // Event Queues
	CTs  ResourceLimits // Counting Triggers
	TLEs ResourceLimits // Trigger List Entries
	PTEs ResourceLimits // Portal Table Entries
	LEs  ResourceLimits // List Entries
	ACs  ResourceLimits // Address Contexts
}

// DefaultResourceSet returns the per-service resource reservation for a single
// CXI service. Only the Res fields are meaningful here — they represent how
// much of each resource type one service consumes. The Max fields are left zero
// because this is a request, not a capacity descriptor.
//
// Values are taken from the Slurm HPE Slingshot switch plugin
// (src/plugins/switch/hpe_slingshot/switch_hpe_slingshot.h) and match the
// caps-tum CNI reference implementation.
func DefaultResourceSet() ResourceSet {
	return ResourceSet{
		TXQs: ResourceLimits{Res: 2},
		TGQs: ResourceLimits{Res: 1},
		EQs:  ResourceLimits{Res: 2},
		CTs:  ResourceLimits{Res: 1},
		TLEs: ResourceLimits{Res: 1},
		PTEs: ResourceLimits{Res: 6},
		LEs:  ResourceLimits{Res: 1},
		ACs:  ResourceLimits{Res: 4},
	}
}

// Add returns a new ResourceSet with the Res fields summed. Used by the
// ResourceTracker to accumulate reserved resources across active services.
func (r ResourceSet) Add(other ResourceSet) ResourceSet {
	return ResourceSet{
		TXQs: ResourceLimits{Max: r.TXQs.Max, Res: r.TXQs.Res + other.TXQs.Res},
		TGQs: ResourceLimits{Max: r.TGQs.Max, Res: r.TGQs.Res + other.TGQs.Res},
		EQs:  ResourceLimits{Max: r.EQs.Max, Res: r.EQs.Res + other.EQs.Res},
		CTs:  ResourceLimits{Max: r.CTs.Max, Res: r.CTs.Res + other.CTs.Res},
		TLEs: ResourceLimits{Max: r.TLEs.Max, Res: r.TLEs.Res + other.TLEs.Res},
		PTEs: ResourceLimits{Max: r.PTEs.Max, Res: r.PTEs.Res + other.PTEs.Res},
		LEs:  ResourceLimits{Max: r.LEs.Max, Res: r.LEs.Res + other.LEs.Res},
		ACs:  ResourceLimits{Max: r.ACs.Max, Res: r.ACs.Res + other.ACs.Res},
	}
}

// Sub returns a new ResourceSet with the Res fields decremented. Used by the
// ResourceTracker when a service is released.
func (r ResourceSet) Sub(other ResourceSet) ResourceSet {
	return ResourceSet{
		TXQs: ResourceLimits{Max: r.TXQs.Max, Res: r.TXQs.Res - other.TXQs.Res},
		TGQs: ResourceLimits{Max: r.TGQs.Max, Res: r.TGQs.Res - other.TGQs.Res},
		EQs:  ResourceLimits{Max: r.EQs.Max, Res: r.EQs.Res - other.EQs.Res},
		CTs:  ResourceLimits{Max: r.CTs.Max, Res: r.CTs.Res - other.CTs.Res},
		TLEs: ResourceLimits{Max: r.TLEs.Max, Res: r.TLEs.Res - other.TLEs.Res},
		PTEs: ResourceLimits{Max: r.PTEs.Max, Res: r.PTEs.Res - other.PTEs.Res},
		LEs:  ResourceLimits{Max: r.LEs.Max, Res: r.LEs.Res - other.LEs.Res},
		ACs:  ResourceLimits{Max: r.ACs.Max, Res: r.ACs.Res - other.ACs.Res},
	}
}

// Fits returns true if adding other's Res values would not exceed this
// ResourceSet's Max values. Used to check admission before creating a service.
func (r ResourceSet) Fits(other ResourceSet) bool {
	return r.TXQs.Res+other.TXQs.Res <= r.TXQs.Max &&
		r.TGQs.Res+other.TGQs.Res <= r.TGQs.Max &&
		r.EQs.Res+other.EQs.Res <= r.EQs.Max &&
		r.CTs.Res+other.CTs.Res <= r.CTs.Max &&
		r.TLEs.Res+other.TLEs.Res <= r.TLEs.Max &&
		r.PTEs.Res+other.PTEs.Res <= r.PTEs.Max &&
		r.LEs.Res+other.LEs.Res <= r.LEs.Max &&
		r.ACs.Res+other.ACs.Res <= r.ACs.Max
}
// DeviceCapacity returns the total hardware resource limits for a CXI device
// when real values cannot be read from hardware.
func DeviceCapacity() ResourceSet {
return ResourceSet{
TXQs: ResourceLimits{Max: 1024},
TGQs: ResourceLimits{Max: 512},
EQs:  ResourceLimits{Max: 2047},
CTs:  ResourceLimits{Max: 2047},
TLEs: ResourceLimits{Max: 2048},
PTEs: ResourceLimits{Max: 2048},
LEs:  ResourceLimits{Max: 16384},
ACs:  ResourceLimits{Max: 1022},
}
}
