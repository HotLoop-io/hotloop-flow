//go:build !linux

package diagram

import "fmt"

// applyFlow: flow control is set on Linux. Elsewhere a port with none is as it
// was, and a diagram that asks for more is refused rather than run without it.
func applyFlow(device, flow string) error {
	if flow == "" || flow == "none" {
		return nil
	}
	return fmt.Errorf("flow control %q is only supported on Linux, and %s was asked for it", flow, device)
}
