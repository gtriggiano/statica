package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// A closed standard output must not end the process with SIGPIPE: writes
// fail with EPIPE instead, and the command exits through its normal error
// path (status 1, with a message on standard error when it is open).
func TestClosedStdoutDoesNotKillTheProcess(t *testing.T) {
	if os.Getenv("STATICA_TEST_MAIN") == "1" {
		os.Args = []string{"statica", "version"}
		main()
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	defer w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestClosedStdoutDoesNotKillTheProcess$")
	cmd.Env = append(os.Environ(), "STATICA_TEST_MAIN=1")
	cmd.Stdout = w
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			t.Fatalf("process killed by %v", status.Signal())
		}
	} else if err != nil {
		t.Fatal(err)
	}
}
