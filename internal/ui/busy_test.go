package ui

import "testing"

// Commands that never touch the running agent may run during a turn; the
// rest wait until it is idle.
func TestBusySafeCommands(t *testing.T) {
	for _, c := range []string{"/menu", "/help", "/theme", "/config", "/stats", "/tools", "/map", "/handoff", "/copy", "/queue", "/clients", "/detach", "/coworkers", "/consult", "/quit", "/exit", "/q"} {
		if !BusySafeCommand(c) {
			t.Errorf("%s should be allowed while busy", c)
		}
	}
	for _, c := range []string{"/model", "/provider", "/sessions", "/resume", "/plan", "/undo", "/verify", "/commit", "/init", "/compact", "/clear", "/custom"} {
		if BusySafeCommand(c) {
			t.Errorf("%s must wait until the agent is idle", c)
		}
	}
	if !BusySafeCommand("/theme dracula") || BusySafeCommand("/model foo") {
		t.Fatal("arguments must not change the decision")
	}
}
