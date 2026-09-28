package cni

import "testing"

func TestPodUIDFromArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		want    string
		wantErr bool
	}{
		{
			name: "valid with extra fields",
			args: "IgnoreUnknown=1;K8S_POD_INFRA_CONTAINER_ID=abc;K8S_POD_UID=uid-123;K8S_POD_NAMESPACE=default",
			want: "uid-123",
		},
		{
			name: "only pod uid",
			args: "K8S_POD_UID=uid-456",
			want: "uid-456",
		},
		{
			name:    "missing pod uid",
			args:    "IgnoreUnknown=1;K8S_POD_NAMESPACE=default",
			wantErr: true,
		},
		{
			name:    "empty value",
			args:    "K8S_POD_UID=",
			wantErr: true,
		},
		{
			name:    "empty args",
			args:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PodUIDFromArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("PodUIDFromArgs(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("PodUIDFromArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
