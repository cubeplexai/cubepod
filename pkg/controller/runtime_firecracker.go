package controller

import "strings"

const (
	// RuntimeClassKataFC is the production Firecracker RuntimeClass name.
	RuntimeClassKataFC = "kata-fc"
	// RuntimeClassKataFCE2E is the Firecracker RuntimeClass used by Kata E2E.
	RuntimeClassKataFCE2E = "kata-fc-e2e"
	// LabelHypervisor is the RuntimeClass label that identifies the hypervisor.
	LabelHypervisor = "cubepod.io/hypervisor"
	// HypervisorFirecracker is the label value for Firecracker.
	HypervisorFirecracker = "firecracker"
)

// IsFirecrackerRuntimeName reports whether a RuntimeClass name or handler is Firecracker.
func IsFirecrackerRuntimeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.EqualFold(n, "none") || n == "-" {
		return false
	}
	return n == RuntimeClassKataFC || n == RuntimeClassKataFCE2E
}

// IsFirecrackerRuntime reports whether the resolved RuntimeClass is Firecracker.
// A RuntimeClass is Firecracker when its name or handler is kata-fc / kata-fc-e2e,
// or when it carries the label cubepod.io/hypervisor=firecracker.
func IsFirecrackerRuntime(name, handler string, labels map[string]string) bool {
	if IsFirecrackerRuntimeName(name) || IsFirecrackerRuntimeName(handler) {
		return true
	}
	if labels != nil && strings.EqualFold(strings.TrimSpace(labels[LabelHypervisor]), HypervisorFirecracker) {
		return true
	}
	return false
}

// IsKnownNonFirecrackerRuntimeName reports well-known non-Firecracker runtime
// class names that do not require a RuntimeClass API lookup.
func IsKnownNonFirecrackerRuntimeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || n == "none" || n == "-" {
		return true
	}
	switch n {
	case "runc", "crun", "runsc", "gvisor", "kata-qemu", "kata-clh", "kata-qemu-nvidia", "kata-clh-nvidia":
		return true
	}
	return strings.HasPrefix(n, "kata-qemu") || strings.HasPrefix(n, "kata-clh")
}
