package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcStopTerminatesEntireProcessGroup(t *testing.T) {
	temporaryDirectory := t.TempDir()
	childPIDPath := filepath.Join(temporaryDirectory, "child.pid")
	scriptPath := writeExecutable(t, temporaryDirectory, "process-tree.sh", `#!/bin/sh
sleep 30 &
echo "$!" > "$CHILD_PID_FILE"
wait
`)
	t.Setenv("CHILD_PID_FILE", childPIDPath)

	process, err := launch([]string{scriptPath}, filepath.Join(temporaryDirectory, "cache"))
	if err != nil {
		t.Fatalf("launch process tree: %v", err)
	}
	t.Cleanup(process.stop)

	childPID := waitForPID(t, childPIDPath)
	process.stop()

	if process.running() {
		t.Fatal("Cog leader still runs after stopping its process group")
	}
	if processExists(childPID) {
		t.Fatalf("Cog child process %d survived the process-group stop", childPID)
	}
}

func TestPID1CrashRecoveryReapsOrphanedChildren(t *testing.T) {
	if os.Getpid() != 1 {
		t.Skip("run the test binary as container PID 1 to reproduce adopted browser children")
	}
	directory := t.TempDir()
	childPIDPath := filepath.Join(directory, "child.pid")
	script := writeExecutable(t, directory, "orphaned-browser.sh", `#!/bin/sh
sleep 30 &
echo "$!" > "$CHILD_PID_FILE"
wait
`)
	t.Setenv("CHILD_PID_FILE", childPIDPath)
	for generation := range 3 {
		process, err := launch([]string{script}, filepath.Join(directory, "cache"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(process.stop)
		childPID := waitForPID(t, childPIDPath)
		if err := process.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		<-process.exited
		process.stop()
		if processExists(childPID) || processGroupRunning(process.pgid) {
			t.Fatalf("generation %d leaked adopted child %d or its process group", generation, childPID)
		}
		if err := os.Remove(childPIDPath); err != nil {
			t.Fatal(err)
		}
	}
}
