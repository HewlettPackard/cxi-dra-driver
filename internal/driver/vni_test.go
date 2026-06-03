package driver

import "testing"

// M3+L2: the VNI pool range must be bounded to the usable fabric VNI space
// (1..65535). Values <1 publish the reserved vni-0; values >65535 silently
// truncate to a uint16 at the libcxi boundary, colliding with low VNIs.
func TestValidateVNIRange(t *testing.T) {
	tests := []struct {
		name       string
		start, end int
		wantErr    bool
	}{
		{"typical", 100, 300, false},
		{"single", 100, 100, false},
		{"min boundary", 1, 1, false},
		{"max boundary", 65535, 65535, false},
		{"start zero (vni-0)", 0, 300, true},
		{"end overflows uint16", 100, 70000, true},
		{"start after end", 300, 100, true},
		{"negative start", -1, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateVNIRange(tt.start, tt.end); (err != nil) != tt.wantErr {
				t.Errorf("ValidateVNIRange(%d, %d) error = %v, wantErr %v", tt.start, tt.end, err, tt.wantErr)
			}
		})
	}
}

func TestValidVNI(t *testing.T) {
	for _, tc := range []struct {
		vni  int
		want bool
	}{
		{1, true}, {100, true}, {65535, true},
		{0, false}, {-1, false}, {65536, false}, {70000, false},
	} {
		if got := ValidVNI(tc.vni); got != tc.want {
			t.Errorf("ValidVNI(%d) = %v, want %v", tc.vni, got, tc.want)
		}
	}
}
