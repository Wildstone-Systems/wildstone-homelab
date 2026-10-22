//go:build !linux

package agent

import wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

// Wardstone targets Linux; this stub only keeps the module compiling on
// developer machines. There is no sysfs to read, so the answer is an
// empty, successful inventory that says so — not an error, since the op
// is informational.
func (a *Agent) listRadios() (*wsv1.OpResult_ListRadios, *wsv1.OpError) {
	return &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{PlatformSupported: false}}, nil
}
