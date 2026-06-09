package cdi

import "testing"

func TestDeviceIDs(t *testing.T) {
	const driver = "cxi.slingshot.hpe.com"
	tests := []struct {
		name string
		fn   func(string) string
		want string
	}{
		{"env", EnvDeviceID, driver + "/env=env"},
		{"nccl", NCCLDeviceID, driver + "/nccl=nccl"},
		{"aws-ofi-nccl", AWSofiNCCLDeviceID, driver + "/aws-ofi-nccl=aws-ofi-nccl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(driver); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
