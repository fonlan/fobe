package agent

import (
	"path/filepath"
	"testing"
)

func TestMachineIDPathForFollowsConfigDir(t *testing.T) {
	cases := []struct {
		config string
		want   string
	}{
		{"", filepath.Join("/etc/fobe-agent", "machine-id")},
		{DefaultConfigPath, filepath.Join("/etc/fobe-agent", "machine-id")},
		{"/opt/fobe-agent/config.json", "/opt/fobe-agent/machine-id"},
		{"relative/config.json", "relative/machine-id"},
	}
	for _, tc := range cases {
		if got := MachineIDPathFor(tc.config); got != tc.want {
			t.Fatalf("MachineIDPathFor(%q) = %q, want %q", tc.config, got, tc.want)
		}
	}
}
