package server

import (
	"testing"

	"github.com/tiny-systems/module/module"
)

func newTestComponent() *Component {
	return (&Component{}).Instance().(*Component)
}

func hasPort(ports []module.Port, name string) bool {
	for _, p := range ports {
		if p.Name == name {
			return true
		}
	}
	return false
}

// TestStopPortGatedBySetting is the user-facing contract: the Stop port exists
// only when EnableStopPort is set, mirroring EnableStatusPort.
func TestStopPortGatedBySetting(t *testing.T) {
	h := newTestComponent()

	if hasPort(h.Ports(), StopPort) {
		t.Fatal("Stop port present when EnableStopPort is false (should be hidden by default)")
	}

	h.settings.EnableStopPort = true
	ports := h.Ports()
	if !hasPort(ports, StopPort) {
		t.Fatal("Stop port missing when EnableStopPort is true")
	}

	// Toggling Stop must not disturb the independent Status port gate.
	if hasPort(ports, StatusPort) {
		t.Fatal("Status port leaked on when only EnableStopPort was set")
	}
}
