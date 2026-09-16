package main

import (
	"os"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/shellcli"
)

func main() {
	shellcli.Run(os.Args[1:])
}
