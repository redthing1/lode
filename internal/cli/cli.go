// Package cli provides Lode's command-line interface.
package cli

import (
	"fmt"
	"io"
)

const usage = `Lode versions large binary projects.

Usage:
  lode <command>

Commands:
  help       Show this help
  version    Show the build version

Options:
  -h, --help    Show this help
  --version     Show the build version
`

// Run executes a command and returns its process exit status.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	command := "help"
	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "help", "-h", "--help", "version", "--version":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "lode: %s takes no arguments\n", command)
			return 2
		}
	default:
		fmt.Fprintf(stderr, "lode: unknown command %q\nRun 'lode help' for usage.\n", command)
		return 2
	}

	var err error
	if command == "version" || command == "--version" {
		_, err = fmt.Fprintf(stdout, "lode %s\n", version)
	} else {
		_, err = io.WriteString(stdout, usage)
	}
	if err != nil {
		fmt.Fprintf(stderr, "lode: %v\n", err)
		return 1
	}
	return 0
}
