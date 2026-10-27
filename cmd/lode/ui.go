package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/model"
	"golang.org/x/sys/unix"
)

// The same descriptions serve the command list and each command's help.
// Execution and flag parsing stay in run; this is presentation data only.
type commandDescription struct{ usage, description string }

var commandHelp = []commandDescription{
	{"init DIR", "Create a server project and print its access token."},
	{"serve [-listen HOST:PORT] DIR", "Serve a project on loopback; use a TLS proxy for remote access."},
	{"attach [options] URL [DIR]", "Track an existing folder in an empty project; DIR defaults to the workspace directory."},
	{"clone [options] URL DIR", "Download selected paths into a new workspace directory."},
	{"status", "Show the workspace branch, base, selection and local changes."},
	{"save -m MESSAGE [-author NAME]", "Save selected changes while preserving unloaded paths."},
	{"update [options]", "Update to the branch tip while preserving independent local edits."},
	{"select [options]", "Show selection, replace it with include/exclude rules, or unload with -none."},
	{"log [-n COUNT]", "List saved versions along the workspace branch's first-parent history."},
	{"branch [-delete] [NAME [COMMIT]]", "List branches, create NAME at COMMIT or the workspace base, or delete NAME."},
	{"switch [options] BRANCH", "Switch branches while preserving independent local edits."},
	{"merge [options] BRANCH", "Merge saved trees on the server; run update afterwards to materialize the result."},
	{"restore [-force] COMMIT [PATH...]", "Restore selected historical contents as local changes, keeping the current base."},
	{"tag [-delete] [NAME [COMMIT]]", "List retention tags, protect COMMIT or the workspace base, or delete NAME."},
	{"cleanup -keep N [-apply TOKEN]", "Preview expiry, or apply its token and collect. Tips and tags always stay protected."},
	{"collect", "Request native storage collection without changing retention."},
	{"backup SOURCE DEST", "Copy a stopped project into a new directory; use the same command to restore."},
	{"resume", "Finish an interrupted save or local transition."},
	{"cancel", "Cancel an unaccepted save or roll back an unfinished local transition."},
}

func helpFor(command string) (commandDescription, bool) {
	for _, info := range commandHelp {
		name, _, _ := strings.Cut(info.usage, " ")
		if name == command {
			return info, true
		}
	}
	return commandDescription{}, false
}

func printHelp(out io.Writer) {
	fmt.Fprint(out, "Lode — version large binary projects\n\nUsage: lode [-C workspace] COMMAND [options]\n\n")
	for _, info := range commandHelp {
		fmt.Fprintf(out, "  %-39s %s\n", info.usage, info.description)
	}
	fmt.Fprintln(out, "\nUse lode help COMMAND for arguments and options. Options precede positional arguments.\nSet LODE_TOKEN for attach/clone and LODE_AUTHOR for the default author.")
}

func selectionText(selection model.Selection) string {
	if len(selection) == 0 {
		return "no paths"
	}
	if len(selection) == 1 && selection[0].Path == "" && selection[0].Include {
		return "all paths"
	}
	var rules []string
	for _, rule := range selection {
		prefix, name := "-", rule.Path
		if rule.Include {
			prefix = "+"
		}
		if name == "" {
			name = "."
		}
		rules = append(rules, prefix+strconv.Quote(name))
	}
	return strings.Join(rules, " ")
}

// Long commands report elapsed activity on a terminal only. No progress state
// enters the application model, and redirected output stays suitable for scripts.
func showActivity(out io.Writer, command string) func() {
	file, ok := out.(*os.File)
	if !ok || command == "serve" || os.Getenv("TERM") == "dumb" {
		return func() {}
	}
	if _, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS); err != nil {
		return func() {}
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		start := time.Now()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(out, "lode %s: working (%s)\n", command, time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() { close(done); <-stopped }
}
