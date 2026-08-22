package microvmtest

import "testing"

func TestDecideKVMSource(t *testing.T) {
	tests := []struct {
		name           string
		kvmOpenable    bool
		requireMicroVM string
		want           kvmSource
	}{
		{
			name:        "kvm openable proceeds",
			kvmOpenable: true,
			want:        sourceProceed,
		},
		{
			name:        "kvm absent no require skips",
			kvmOpenable: false,
			want:        sourceSkipNoKVM,
		},
		{
			name:           "kvm absent require flag fails",
			kvmOpenable:    false,
			requireMicroVM: "1",
			want:           sourceFailRequire,
		},
		{
			name:           "require flag never suppresses proceed when kvm present",
			kvmOpenable:    true,
			requireMicroVM: "1",
			want:           sourceProceed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideKVMSource(tt.kvmOpenable, tt.requireMicroVM); got != tt.want {
				t.Errorf("decideKVMSource(%t, %q) = %d, want %d",
					tt.kvmOpenable, tt.requireMicroVM, got, tt.want)
			}
		})
	}
}
