package controller

import "testing"

func TestIsFirecrackerRuntime(t *testing.T) {
	cases := []struct {
		name, handler string
		labels        map[string]string
		want          bool
	}{
		{name: "kata-fc", want: true},
		{name: "kata-fc-e2e", want: true},
		{name: "KATA-FC", want: true},
		{name: "kata-qemu", want: false},
		{name: "kata-clh", want: false},
		{name: "", want: false},
		{name: "none", want: false},
		{name: "custom", handler: "kata-fc", want: true},
		{name: "custom", labels: map[string]string{LabelHypervisor: "firecracker"}, want: true},
		{name: "custom", labels: map[string]string{LabelHypervisor: "qemu"}, want: false},
	}
	for _, tc := range cases {
		if got := IsFirecrackerRuntime(tc.name, tc.handler, tc.labels); got != tc.want {
			t.Fatalf("IsFirecrackerRuntime(%q,%q,%v)=%v want %v", tc.name, tc.handler, tc.labels, got, tc.want)
		}
	}
}
