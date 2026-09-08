package main

import (
	"os"

	"github.com/pathcl/notrace/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
