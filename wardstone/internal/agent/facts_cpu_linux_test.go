package agent

import "testing"

// A real sample is reported as measured, whatever it reads — an idle
// machine at 0% included — so the household's card keeps its CPU line.
func TestCPUBusyIsReportedAsMeasured(t *testing.T) {
	busy, measured := cpuBusyPercent()
	if !measured {
		t.Fatal("a readable /proc/stat was reported as not measured")
	}
	if busy > 100 {
		t.Errorf("busy = %d%%", busy)
	}
}
