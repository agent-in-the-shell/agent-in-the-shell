package main

import (
	"os"
	"strings"
	"testing"
)

func TestMainHelp(t *testing.T) {
	oldArgs := os.Args
	oldStdout := os.Stdout
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	os.Args = []string{"agent-shell", "--help"}
	os.Stdout = stdout
	defer func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
	}()

	main()

	if _, err := stdout.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "agent-shell: unified CLI") {
		t.Fatalf("help output = %q", body)
	}
}
