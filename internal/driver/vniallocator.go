package driver

// nextFreeVNI returns the lowest VNI in [start, end] that is not present in
// assigned, or (0, false) if the range is exhausted.
func nextFreeVNI(assigned map[int]bool, start, end int) (int, bool) {
	for vni := start; vni <= end; vni++ {
		if !assigned[vni] {
			return vni, true
		}
	}
	return 0, false
}
