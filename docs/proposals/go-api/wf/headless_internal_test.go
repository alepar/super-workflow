package wf

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// A headless child is its own process session: it never dies with a
// replaced coordinator, whatever its harness's subagents do.
func TestHeadlessChildOutlivesItsCoordinator(t *testing.T) {
	sub := &unit{Claim: &unitClaim{Harness: "claude"}}
	headless := &unit{Claim: &unitClaim{Harness: "claude"}, ChildHarness: "claude"}
	if !childDiesWithCoordinator(sub) || childDiesWithCoordinator(headless) {
		t.Errorf("subagent dies %v (want true), headless dies %v (want false)", childDiesWithCoordinator(sub), childDiesWithCoordinator(headless))
	}
}

// Kill reaches the child's tool processes after the child itself exited:
// its process group is signalled while members of it remain.
func TestKillReachesTheGroupAfterItsLeaderExited(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("group members are read from /proc")
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start := procStart(pid)
	deadline := time.Now().Add(5 * time.Second)
	for !groupMembers(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // the leader alone
	_ = cmd.Wait()
	if !groupMembers(pid) {
		t.Fatal("the sleeps left the group before the test could check it")
	}
	if err := (osProcess{}).Kill(pid, start); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for groupMembers(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if groupMembers(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatal("the group's members outlived Kill")
	}
}
