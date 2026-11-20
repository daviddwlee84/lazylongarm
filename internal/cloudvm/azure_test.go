package cloudvm

import "testing"

func TestDiskTier(t *testing.T) {
	cases := []struct {
		sku  string
		gb   int
		want string
	}{
		{"StandardSSD_LRS", 30, "E4 LRS Disk"},
		{"StandardSSD_LRS", 64, "E6 LRS Disk"},
		{"Premium_LRS", 30, "P4 LRS Disk"},
		{"Standard_LRS", 8, "S4 LRS Disk"},
		{"Standard_LRS", 100, "S10 LRS Disk"},
		{"UltraSSD_LRS", 30, ""},
	}
	for _, c := range cases {
		if got := diskTier(c.sku, c.gb); got != c.want {
			t.Errorf("diskTier(%s, %d) = %q, want %q", c.sku, c.gb, got, c.want)
		}
	}
}

func TestOwnedRGNeedsBothTags(t *testing.T) {
	if !OwnedRG(map[string]string{"managed-by": "lazylongarm", "purpose": "longarm-hub"}) {
		t.Fatal("tagged group not owned")
	}
	for _, tags := range []map[string]string{nil, {"managed-by": "lazylongarm"}, {"purpose": "longarm-hub"}, {"managed-by": "someone", "purpose": "longarm-hub"}} {
		if OwnedRG(tags) {
			t.Fatalf("%v treated as owned", tags)
		}
	}
}

func TestStateHelpers(t *testing.T) {
	if !(VM{Power: "VM running"}).Running() || !(VM{Power: "VM deallocated"}).Deallocated() || (VM{Power: "VM stopped"}).Deallocated() {
		t.Fatal("power state helpers")
	}
}
