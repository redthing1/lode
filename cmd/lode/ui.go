package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/model"
)

// The same descriptions serve the command list and each command's help.
// Execution and flag parsing stay in run; this is presentation data only.
type commandDescription struct{ usage, description string }

var commandHelp = []commandDescription{
	{"login URL", "Verify an access token and save it for the instance or project URL."},
	{"logout URL", "Remove the saved credential for the instance or project URL."},
	{"init [-url URL] DIR", "Create an instance and print its one-time setup code."},
	{"import INSTANCE OWNER/PROJECT PROJECT_STORE", "Import an existing native project store for an account."},
	{"recover INSTANCE USERNAME", "Reset an account while the server is stopped."},
	{"serve [-listen HOST:PORT] DIR", "Serve an instance on loopback; use a TLS proxy for remote access."},
	{"attach [options] URL [DIR]", "Track an existing folder in an empty project; DIR defaults to the current directory or -C DIR."},
	{"clone [options] URL DIR", "Download selected paths into a new workspace directory."},
	{"status", "Show the workspace branch, base, selection and local changes."},
	{"save -m MESSAGE", "Save selected changes while preserving unloaded paths."},
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
	{"backup SOURCE DEST", "Copy a stopped instance and its projects into a new directory; use the same command to restore."},
	{"resume", "Finish an interrupted save or local transition."},
	{"cancel", "Cancel an unaccepted save or roll back an unfinished local transition."},
	{"version", "Print the installed version."},
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
	fmt.Fprintln(out, "\nRun workspace commands from any directory inside a workspace, or use -C DIR.\nWorkspace paths are relative to the root; attach/clone destinations use the current directory. Options precede positional arguments.\nUse lode help COMMAND for options, or lode --version for the installed version.\nUse lode login URL to save credentials; LODE_TOKEN overrides saved credentials.")
}

func printVersion(out io.Writer) {
	fmt.Fprintln(out, "lode", buildVersion())
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
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
	if !isTerminal(file) {
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
