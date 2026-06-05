package cxi

import "fmt"

// TLEPoolSize is the hardware allocation granularity for Trigger List Entries.
// TLEs are allocated in pools of this size — reserving even 1 TLE consumes a
// full pool. Defined once here; parameterise if future hardware versions differ.
const TLEPoolSize = 512

// LEPoolSize is the hardware allocation granularity for List Entries.
// LEs are allocated in pools of this size — reserving even 1 LE consumes a
// full pool. Defined once here; parameterise if future hardware versions differ.
const LEPoolSize = 1024

// ResourceSet describes a set of CXI hardware resources. It is used in three
// contexts:
//
//  1. Hardware total — the raw device maxima from cxil_devinfo.
//  2. User request — the resources a pod is asking for (from claim config or
//     driver defaults). TLEs and LEs are expressed as pool counts, converted
//     to absolute values before service creation.
//  3. Available — hardware total minus sum of reserved across all live services,
//     with LE/TLE rounded up to pool boundaries.
//
// Field names match the cxi_svc_desc resource names used by libcxi and the
// cxi_service CLI.
type ResourceSet struct {
	TXQs int64 // Transmit Queues
	TGQs int64 // Target (Get) Queues
	EQs  int64 // Event Queues
	CTs  int64 // Counting Triggers
	TLEs int64 // Trigger List Entries (absolute value; pool-rounded externally)
	PTEs int64 // Portal Table Entries
	LEs  int64 // List Entries (absolute value; pool-rounded externally)
	ACs  int64 // Address Contexts
}

// DeviceCapacity returns the total hardware resource limits for a CXI device.
// Used in non-cxi builds (CI, mock mode). Production builds read real values
// from cxil_devinfo — see capacity_cgo.go.
func DeviceCapacity() ResourceSet {
	return ResourceSet{
		TXQs: 1024,
		TGQs: 512,
		EQs:  2047,
		CTs:  2047,
		TLEs: 2048,
		PTEs: 2048,
		LEs:  16384,
		ACs:  1022,
	}
}

// PoolsToLEs converts a pool count to an absolute LE count.
func PoolsToLEs(pools int64) int64 {
	return pools * LEPoolSize
}

// PoolsToTLEs converts a pool count to an absolute TLE count.
func PoolsToTLEs(pools int64) int64 {
	return pools * TLEPoolSize
}

// RoundUpToLEPool rounds an absolute LE count up to the nearest pool boundary.
// Used when computing available capacity from live reserved totals.
func RoundUpToLEPool(les int64) int64 {
	if les == 0 {
		return 0
	}
	return ((les + LEPoolSize - 1) / LEPoolSize) * LEPoolSize
}

// RoundUpToTLEPool rounds an absolute TLE count up to the nearest pool boundary.
func RoundUpToTLEPool(tles int64) int64 {
	if tles == 0 {
		return 0
	}
	return ((tles + TLEPoolSize - 1) / TLEPoolSize) * TLEPoolSize
}

// Fits returns true if r (a request) fits within available. For LEs and TLEs
// the check is against pool-rounded values — the caller is responsible for
// passing an available ResourceSet with LEs/TLEs already rounded up to pool
// boundaries (i.e. the true consumed pool space).
func (r ResourceSet) Fits(available ResourceSet) bool {
	return r.TXQs <= available.TXQs &&
		r.TGQs <= available.TGQs &&
		r.EQs <= available.EQs &&
		r.CTs <= available.CTs &&
		r.TLEs <= available.TLEs &&
		r.PTEs <= available.PTEs &&
		r.LEs <= available.LEs &&
		r.ACs <= available.ACs
}

// ClampToZero returns a copy of r with any negative fields set to zero.
// Used when subtracting overhead from available capacity to avoid publishing
// negative attribute values if overhead exceeds what the hardware reports.
func (r ResourceSet) ClampToZero() ResourceSet {
	clamp := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		return v
	}
	return ResourceSet{
		TXQs: clamp(r.TXQs),
		TGQs: clamp(r.TGQs),
		EQs:  clamp(r.EQs),
		CTs:  clamp(r.CTs),
		TLEs: clamp(r.TLEs),
		PTEs: clamp(r.PTEs),
		LEs:  clamp(r.LEs),
		ACs:  clamp(r.ACs),
	}
}

// Sub returns available minus reserved — used to compute remaining capacity
// after summing all live service reservations.
func (r ResourceSet) Sub(other ResourceSet) ResourceSet {
	return ResourceSet{
		TXQs: r.TXQs - other.TXQs,
		TGQs: r.TGQs - other.TGQs,
		EQs:  r.EQs - other.EQs,
		CTs:  r.CTs - other.CTs,
		TLEs: r.TLEs - other.TLEs,
		PTEs: r.PTEs - other.PTEs,
		LEs:  r.LEs - other.LEs,
		ACs:  r.ACs - other.ACs,
	}
}

// Add returns the sum of two ResourceSets — used to accumulate reserved totals
// across all live services.
func (r ResourceSet) Add(other ResourceSet) ResourceSet {
	return ResourceSet{
		TXQs: r.TXQs + other.TXQs,
		TGQs: r.TGQs + other.TGQs,
		EQs:  r.EQs + other.EQs,
		CTs:  r.CTs + other.CTs,
		TLEs: r.TLEs + other.TLEs,
		PTEs: r.PTEs + other.PTEs,
		LEs:  r.LEs + other.LEs,
		ACs:  r.ACs + other.ACs,
	}
}

// String returns a compact human-readable representation, useful for logging.
func (r ResourceSet) String() string {
	return fmt.Sprintf(
		"txqs=%d tgqs=%d eqs=%d cts=%d ptes=%d les=%d tles=%d acs=%d",
		r.TXQs, r.TGQs, r.EQs, r.CTs, r.PTEs, r.LEs, r.TLEs, r.ACs,
	)
}
