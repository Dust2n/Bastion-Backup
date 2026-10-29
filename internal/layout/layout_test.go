package layout

import "testing"

func winDisk() Disk {
	return Disk{
		Number: 0, Size: 500 * GiB, Style: "gpt",
		Partitions: []Partition{
			{Number: 1, Offset: 1 * MiB, Size: 260 * MiB, Role: RoleESP, FS: "fat32"},
			{Number: 2, Offset: 261 * MiB, Size: 16 * MiB, Role: RoleMSR},
			{Number: 3, Offset: 277 * MiB, Size: 498 * GiB, Role: RoleSystem, FS: "ntfs", Mount: "C:", Used: 80 * GiB, Volume: "C:"},
			{Number: 4, Offset: 498*GiB + 277*MiB, Size: 900 * MiB, Role: RoleRecovery},
		},
	}
}

func total(p []Planned) int64 {
	var n int64
	for _, x := range p {
		n += x.Size
	}
	return n
}

func TestShrinkToSmallerDisk(t *testing.T) {
	plan, err := Scale(winDisk(), 128*GiB)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 {
		t.Fatalf("expected ESP, MSR, C: (recovery dropped), got %d", len(plan))
	}
	c := plan[2]
	if c.Source.Mount != "C:" || c.Size < 88*GiB {
		t.Fatalf("C: too small: %s", Human(c.Size))
	}
	if total(plan) > 128*GiB-2*MiB {
		t.Fatalf("plan exceeds disk: %s", Human(total(plan)))
	}
	for _, p := range plan {
		if p.Size%MiB != 0 {
			t.Fatal("not aligned")
		}
	}
}

func TestTooSmall(t *testing.T) {
	if _, err := Scale(winDisk(), 64*GiB); err == nil {
		t.Fatal("expected error for 64 GiB target with 80 GiB used")
	}
	if Required(winDisk()) <= 88*GiB {
		t.Fatalf("Required too low: %s", Human(Required(winDisk())))
	}
}

func TestBackupSizesRaiseMinimum(t *testing.T) {
	d := winDisk()
	before := Required(d)
	after := Required(WithBackupSizes(d, map[string]int64{"C:": 90 * GiB}))
	if after <= before {
		t.Fatalf("uncompressed backup size must raise the minimum: %s -> %s", Human(before), Human(after))
	}
	if d.Partitions[2].Used != 80*GiB {
		t.Fatal("original layout must not be modified")
	}
}

func TestGrowToLargerDisk(t *testing.T) {
	plan, err := Scale(winDisk(), 1000*GiB)
	if err != nil {
		t.Fatal(err)
	}
	if plan[2].Size < 990*GiB {
		t.Fatalf("C: should grow, got %s", Human(plan[2].Size))
	}
}

func TestLinuxMultiplePartitions(t *testing.T) {
	d := Disk{Size: 200 * GiB, Style: "gpt", Partitions: []Partition{
		{Offset: 1 * MiB, Size: 512 * MiB, Role: RoleESP, Volume: "/boot/efi"},
		{Offset: 513 * MiB, Size: 1 * GiB, Role: RoleBoot, Used: 200 * MiB, Volume: "/boot"},
		{Offset: 2 * GiB, Size: 100 * GiB, Role: RoleSystem, Used: 20 * GiB, Volume: "/"},
		{Offset: 102 * GiB, Size: 90 * GiB, Role: RoleData, Used: 10 * GiB, Volume: "/home"},
		{Offset: 192 * GiB, Size: 8 * GiB, Role: RoleSwap},
	}}
	plan, err := Scale(d, 60*GiB)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 5 {
		t.Fatalf("got %d partitions", len(plan))
	}
	for _, p := range plan {
		if p.Size < MinSize(p.Source) && p.Source.Role != RoleESP {
			t.Fatalf("%s below minimum", p.Source.Volume)
		}
	}
	if total(plan) > 60*GiB-2*MiB {
		t.Fatal("plan exceeds disk")
	}
}
