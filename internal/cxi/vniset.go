package cxi

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// vniListDelim wraps the list form of a VNISet so a CEL selector can test
// membership with a plain substring match (",40," never matches inside ",140,").
const vniListDelim = ","

// VNISet is the set of VNIs a CXI parent service allows. The kernel stores it
// in one of two forms and libcxi exposes both:
//
//   - range: [min, max] via cxil_svc_get_vni_range (restricted_vnis unset);
//   - list: up to CXI_SVC_MAX_VNIS explicit values via vnis[] (restricted_vnis set).
//
// A list whose values are contiguous is normalized to the range form, so a
// given set of VNIs always has a single representation and the ResourceSlice
// publishes it as vni_min/vni_max whenever possible. The zero value is the
// empty set.
type VNISet struct {
	kind     vniKind
	min, max uint16   // range form
	list     []uint16 // non-contiguous form: sorted, unique, len >= 2
}

// vniKind is non-zero only for a populated set, so the zero VNISet is empty
// rather than the range 0-0.
type vniKind uint8

const (
	vniEmpty vniKind = iota
	vniRange
	vniList
)

// NewVNIRange returns the set [vniMin, vniMax] (inclusive).
func NewVNIRange(vniMin, vniMax uint16) (VNISet, error) {
	if vniMin > vniMax {
		return VNISet{}, fmt.Errorf("invalid VNI range: min %d > max %d", vniMin, vniMax)
	}
	return VNISet{kind: vniRange, min: vniMin, max: vniMax}, nil
}

// NewVNIList returns the set holding the given VNIs. Duplicates are dropped and
// the order is irrelevant. A contiguous result is stored in range form; an empty
// input yields the empty set.
func NewVNIList(vnis []uint16) VNISet {
	sorted := slices.Clone(vnis)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if len(sorted) == 0 {
		return VNISet{}
	}
	first, last := sorted[0], sorted[len(sorted)-1]
	if int(last)-int(first)+1 == len(sorted) {
		return VNISet{kind: vniRange, min: first, max: last}
	}
	return VNISet{kind: vniList, list: sorted}
}

// IsEmpty reports whether the set contains no VNI.
func (s VNISet) IsEmpty() bool {
	return s.kind == vniEmpty
}

// Range returns the bounds when the set is a single contiguous range. ok is
// false for the empty set and for a non-contiguous list.
func (s VNISet) Range() (vniMin, vniMax uint16, ok bool) {
	if s.kind != vniRange {
		return 0, 0, false
	}
	return s.min, s.max, true
}

// List returns a copy of the values of a non-contiguous set, or nil when the
// set is a range or empty (use Range in that case).
func (s VNISet) List() []uint16 {
	return slices.Clone(s.list)
}

// Contains reports whether vni is in the set.
func (s VNISet) Contains(vni uint16) bool {
	switch s.kind {
	case vniRange:
		return s.min <= vni && vni <= s.max
	case vniList:
		_, found := slices.BinarySearch(s.list, vni)
		return found
	default:
		return false
	}
}

// Covers reports whether every VNI in [start, end] is in the set. Used to check
// a parent against the operator's --vni-range-start/--vni-range-end window. It
// is false when start > end.
func (s VNISet) Covers(start, end uint16) bool {
	if s.kind == vniEmpty || start > end {
		return false
	}
	if s.kind == vniRange {
		return s.min <= start && end <= s.max
	}
	// A list is never contiguous, so a window wider than the list cannot be covered.
	if int(end)-int(start)+1 > len(s.list) {
		return false
	}
	for v := int(start); v <= int(end); v++ {
		if !s.Contains(uint16(v)) {
			return false
		}
	}
	return true
}

// String is the form published in the ResourceSlice: "min-max" for a range
// (the slice carries it as vni_min/vni_max ints) and the delimiter-wrapped
// list ",40,42," for a non-contiguous set (the vni_set attribute). The empty
// set renders as "".
func (s VNISet) String() string {
	switch s.kind {
	case vniList:
		var b strings.Builder
		b.WriteString(vniListDelim)
		for _, v := range s.list {
			b.WriteString(strconv.Itoa(int(v)))
			b.WriteString(vniListDelim)
		}
		return b.String()
	case vniRange:
		return fmt.Sprintf("%d-%d", s.min, s.max)
	default:
		return ""
	}
}
