package shell

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The detach mode follows its inputs (escalation-and-recovery G1): a PID
// namespace owned by a per-command sandbox is none (the runner dies with its
// command), Codex's one-field NSpid included; a container's nested namespace
// is not; otherwise systemd when a user bus is there, else process. The inputs are kept, so a misread is
// diagnosable from the journal.
func TestDetachModeFromItsInputs(t *testing.T) {
	for _, c := range []struct {
		name, status, pid1, systemd, want string
		fields                            int
	}{
		{"host", "Name:\tx\nNSpid:\t4242\n", "systemd", "user bus", wf.DetachSystemd, 1},
		{"no bus", "NSpid:\t4242\n", "systemd", "no user bus", wf.DetachProcess, 1},
		{"bwrap", "NSpid:\t4242\t7\n", "bwrap", "user bus", wf.DetachNone, 2},
		{"codex sandbox", "NSpid:\t4242\n", "codex", "no user bus: exit 1: Failed to connect to user scope bus via local transport: Operation not permitted", wf.DetachNone, 1},
		{"toolbox container", "NSpid:\t4242\t7\n", "toolbox", "user bus", wf.DetachSystemd, 2},
		{"docker container", "NSpid:\t4242\t7\n", "tini", "not found", wf.DetachProcess, 2},
		{"no NSpid", "Name:\tx\n", "", "not found", wf.DetachProcess, 0},
	} {
		got := detachModeOf([]byte(c.status), c.pid1, c.systemd)
		if got.Mode != c.want || got.NSpidFields != c.fields || got.Systemd != c.systemd || got.Pid1 != c.pid1 {
			t.Errorf("%s: %+v, want mode %s, %d NSpid fields, pid1 %q, systemd %q", c.name, got, c.want, c.fields, c.pid1, c.systemd)
		}
	}
}

// WF_DETACH_MODE states the mode for a host the probe misreads.
func TestDetachModeOverride(t *testing.T) {
	r := DetachedRunner(testBin(t)).(wf.DetachProber)
	for _, m := range []string{wf.DetachNone, wf.DetachProcess, wf.DetachSystemd} {
		t.Setenv(detachModeEnv, m)
		if got := r.DetachMode(); got.Mode != m || got.Systemd != "set by "+detachModeEnv {
			t.Errorf("%s=%s: %+v", detachModeEnv, m, got)
		}
	}
	t.Setenv(detachModeEnv, "bogus")
	if got := r.DetachMode(); got.Systemd == "set by "+detachModeEnv {
		t.Errorf("an unknown override was honoured: %+v", got)
	}
}

// The probe proves the bus by running a unit: a bus that is advertised but
// refuses the connection (the Codex sandbox) is no user bus, so the mode is
// not systemd.
func TestDetachProbeRunsAUnit(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	stubSystemdRun(t, "echo 'Failed to connect to user scope bus via local transport: Operation not permitted' >&2\nexit 1\n")
	got := systemdAnswer()
	if got != "no user bus: exit 1: Failed to connect to user scope bus via local transport: Operation not permitted" {
		t.Fatalf("systemd probe = %q, want the probe unit's refusal", got)
	}
	if f := detachModeOf([]byte("NSpid:\t4242\n"), "systemd", got); f.Mode == wf.DetachSystemd {
		t.Fatalf("mode = %+v, want not systemd", f)
	}
}

// Under a per-command sandbox the systemd probe (a unit) is not run at all.
func TestDetachProbeSkippedInASandbox(t *testing.T) {
	got := probeDetach([]byte("NSpid:\t4242\n"), "codex", func() string { t.Fatal("systemd probed under a per-command sandbox"); return "" })
	if got.Mode != wf.DetachNone {
		t.Fatalf("mode = %+v, want none", got)
	}
	if got := probeDetach([]byte("NSpid:\t4242\n"), "systemd", func() string { return "user bus" }); got.Mode != wf.DetachSystemd {
		t.Fatalf("host mode = %+v, want systemd", got)
	}
}
