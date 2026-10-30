//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunAnsibleCancellationKillsDescendantProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	a := &Agent{cfg: Config{PlaybookDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- a.runAnsible(ctx, "/bin/sh", []string{"-c", "sleep 60 & echo $! > \"$1\"; wait", "helper", pidFile}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"})
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("Ansible descendant did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("runAnsible cancellation = %v, context = %v", err, ctx.Err())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("runAnsible did not return within the cancellation bound")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processExitedOrZombie(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived cancellation", pid)
}

func processExitedOrZombie(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A killed orphan may briefly remain as a zombie until the container's
	// PID 1 reaps it. A zombie cannot execute or continue host mutation.
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return os.IsNotExist(err)
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	return closeParen >= 0 && len(stat) > closeParen+2 && stat[closeParen+2] == 'Z'
}
