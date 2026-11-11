package hardware

import "testing"

func TestCPUModel(t *testing.T) {
	for _, tt := range []struct{ name, cpuinfo, want string }{
		{"x64", "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz\n",
			"Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz"},
		{"arm64", "processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\nCPU architecture: 8\nCPU variant\t: 0x3\nCPU part\t: 0xd0c\nCPU revision\t: 1\n\nprocessor\t: 1\nCPU implementer\t: 0x41\nCPU part\t: 0xd0c\n",
			"implementer 0x41 part 0xd0c"},
		{"none", "", ""},
	} {
		if got := cpuModel([]byte(tt.cpuinfo)); got != tt.want {
			t.Errorf("%s: cpuModel = %q, want %q", tt.name, got, tt.want)
		}
	}
}
