package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"runtime"
	"strings"
	"testing"
)

func TestHelpRoutesWithoutOpeningAWorkspace(t *testing.T) {
	commands := []string{
		"auth", "auth login", "auth status", "auth logout", "projects", "attach", "clone", "remote",
		"status", "save", "update", "select", "branch", "branch create", "branch delete",
		"tag", "tag create", "tag delete", "retention", "cleanup", "collect", "switch", "merge",
		"restore", "resume", "cancel", "log", "version",
	}
	if runtime.GOOS == "linux" {
		commands = append(commands, "init", "serve", "backup", "recover-user")
	}
	directory := t.TempDir()
	help := func(t *testing.T, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		err := run(context.Background(), append([]string{"-C", directory}, args...), &out)
		if err != nil && !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.HasPrefix(out.String(), "Usage: lode") {
			t.Fatalf("missing usage for %v: %s", args, &out)
		}
		return out.String()
	}
	if help(t) != help(t, "--help") || help(t, "help") != help(t, "--help") {
		t.Fatal("top-level help differs between entry points")
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			args := strings.Fields(command)
			if help(t, append(append([]string(nil), args...), "--help")...) != help(t, append([]string{"help"}, args...)...) {
				t.Fatal("command help differs between entry points")
			}
		})
	}
}
