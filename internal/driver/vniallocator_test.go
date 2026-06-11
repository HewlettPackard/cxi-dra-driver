package driver

import "testing"

func TestNextFreeVNI(t *testing.T) {
	tests := []struct {
		name     string
		assigned map[int]bool
		start    int
		end      int
		wantVNI  int
		wantOK   bool
	}{
		{"empty range picks start", map[int]bool{}, 10, 12, 10, true},
		{"skips assigned", map[int]bool{10: true, 11: true}, 10, 12, 12, true},
		{"gap in middle", map[int]bool{10: true, 12: true}, 10, 12, 11, true},
		{"exhausted", map[int]bool{10: true, 11: true, 12: true}, 10, 12, 0, false},
		{"single free", map[int]bool{}, 5, 5, 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vni, ok := nextFreeVNI(tc.assigned, tc.start, tc.end)
			if ok != tc.wantOK || (ok && vni != tc.wantVNI) {
				t.Fatalf("nextFreeVNI(%v,%d,%d) = (%d,%v), want (%d,%v)",
					tc.assigned, tc.start, tc.end, vni, ok, tc.wantVNI, tc.wantOK)
			}
		})
	}
}
