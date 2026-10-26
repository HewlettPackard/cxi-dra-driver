package cxi

import (
	"slices"
	"testing"
)

func mustRange(t *testing.T, lo, hi uint16) VNISet {
	t.Helper()
	s, err := NewVNIRange(lo, hi)
	if err != nil {
		t.Fatalf("NewVNIRange(%d, %d): %v", lo, hi, err)
	}
	return s
}

func TestNewVNIRangeInvalid(t *testing.T) {
	if _, err := NewVNIRange(10, 9); err == nil {
		t.Fatal("expected error for min > max")
	}
}

func TestVNISetContains(t *testing.T) {
	tests := []struct {
		name string
		set  VNISet
		vni  uint16
		want bool
	}{
		{"range low edge", mustRange(t, 40, 45), 40, true},
		{"range high edge", mustRange(t, 40, 45), 45, true},
		{"range below", mustRange(t, 40, 45), 39, false},
		{"range above", mustRange(t, 40, 45), 46, false},
		{"list member", NewVNIList([]uint16{40, 42}), 42, true},
		{"list gap", NewVNIList([]uint16{40, 42}), 41, false},
		{"list outside", NewVNIList([]uint16{40, 42}), 140, false},
		{"zero value", VNISet{}, 0, false},
		{"empty list", NewVNIList(nil), 0, false},
		{"range 0-0 is not empty", mustRange(t, 0, 0), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Contains(tt.vni); got != tt.want {
				t.Errorf("Contains(%d) = %v, want %v", tt.vni, got, tt.want)
			}
		})
	}
}

func TestVNISetCovers(t *testing.T) {
	tests := []struct {
		name       string
		set        VNISet
		start, end uint16
		want       bool
	}{
		{"range exact", mustRange(t, 40, 45), 40, 45, true},
		{"range inside", mustRange(t, 40, 45), 41, 43, true},
		{"range single", mustRange(t, 40, 45), 45, 45, true},
		{"range overhang high", mustRange(t, 40, 45), 44, 46, false},
		{"range overhang low", mustRange(t, 40, 45), 39, 41, false},
		{"list single member", NewVNIList([]uint16{40, 42}), 42, 42, true},
		{"list window across gap", NewVNIList([]uint16{40, 42}), 40, 42, false},
		{"list window wider than list", NewVNIList([]uint16{40, 42}), 0, 65535, false},
		{"start after end", mustRange(t, 40, 45), 45, 40, false},
		{"zero value", VNISet{}, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Covers(tt.start, tt.end); got != tt.want {
				t.Errorf("Covers(%d, %d) = %v, want %v", tt.start, tt.end, got, tt.want)
			}
		})
	}
}

func TestNewVNIListNormalizes(t *testing.T) {
	t.Run("contiguous list becomes range", func(t *testing.T) {
		s := NewVNIList([]uint16{42, 40, 41, 41})
		lo, hi, ok := s.Range()
		if !ok || lo != 40 || hi != 42 {
			t.Fatalf("Range() = %d, %d, %v; want 40, 42, true", lo, hi, ok)
		}
		if s.List() != nil {
			t.Errorf("List() = %v, want nil for a range", s.List())
		}
		if got := s.String(); got != "40-42" {
			t.Errorf("String() = %q, want 40-42", got)
		}
	})

	t.Run("non-contiguous list is sorted and deduplicated", func(t *testing.T) {
		s := NewVNIList([]uint16{50, 40, 50, 41})
		if _, _, ok := s.Range(); ok {
			t.Fatal("Range() ok = true for a non-contiguous list")
		}
		if want := []uint16{40, 41, 50}; !slices.Equal(s.List(), want) {
			t.Errorf("List() = %v, want %v", s.List(), want)
		}
		if got := s.String(); got != ",40,41,50," {
			t.Errorf("String() = %q, want ,40,41,50,", got)
		}
	})

	t.Run("input slice is not modified", func(t *testing.T) {
		in := []uint16{50, 40}
		NewVNIList(in)
		if in[0] != 50 || in[1] != 40 {
			t.Errorf("input mutated: %v", in)
		}
	})

	t.Run("List returns a copy", func(t *testing.T) {
		s := NewVNIList([]uint16{40, 50})
		s.List()[0] = 99
		if !s.Contains(40) {
			t.Error("mutating List() result changed the set")
		}
	})

	t.Run("empty", func(t *testing.T) {
		s := NewVNIList(nil)
		if !s.IsEmpty() || s.String() != "" {
			t.Errorf("empty list: IsEmpty=%v String=%q", s.IsEmpty(), s.String())
		}
		if _, _, ok := s.Range(); ok {
			t.Error("Range() ok = true for the empty set")
		}
	})
}

func TestVNISetString(t *testing.T) {
	if got := mustRange(t, 1000, 1999).String(); got != "1000-1999" {
		t.Errorf("range String() = %q", got)
	}
	// The delimiters keep ",40," from matching inside ",140,".
	if got := NewVNIList([]uint16{140, 40}).String(); got != ",40,140," {
		t.Errorf("list String() = %q", got)
	}
}
