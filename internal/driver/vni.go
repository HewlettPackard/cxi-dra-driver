package driver

import "fmt"

const (
	// MinVNI and MaxVNI bound the usable Slingshot fabric VNI space. VNI is a
	// uint16 hardware field (see internal/cxi/service_cgo.go, which casts to
	// uint16), and 0 is reserved — the flags treat 0 as "pool disabled".
	MinVNI = 1
	MaxVNI = 65535
)

// ValidVNI reports whether vni is within the usable fabric VNI range. It guards
// the hardware boundary: an out-of-range VNI would silently truncate to uint16
// and could scope a service to the wrong fabric network.
func ValidVNI(vni int) bool {
	return vni >= MinVNI && vni <= MaxVNI
}

// ValidateVNIRange checks that [start, end] is a usable cluster-wide VNI pool
// range: MinVNI <= start <= end <= MaxVNI.
func ValidateVNIRange(start, end int) error {
	if start < MinVNI || end > MaxVNI || start > end {
		return fmt.Errorf("invalid VNI range [%d, %d]: must satisfy %d <= start <= end <= %d",
			start, end, MinVNI, MaxVNI)
	}
	return nil
}
