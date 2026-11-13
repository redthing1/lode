package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run the installed CLI on a real terminal, keeping stdout separate from prompts.
func terminalCommand(t *testing.T, ctx context.Context, binary string, args []string, steps [][2]string) (string, string, error) {
	t.Helper()
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root := t.TempDir()
	env := append(os.Environ(), "TERM=xterm-256color", "LODE_TEST_BIN="+binary,
		"LODE_TEST_STDOUT="+filepath.Join(root, "stdout"),
		"LODE_TEST_BEFORE="+filepath.Join(root, "before"), "LODE_TEST_AFTER="+filepath.Join(root, "after"))
	command := `stty -g > "$LODE_TEST_BEFORE"
"$LODE_TEST_BIN"`
	for i, arg := range args {
		name := fmt.Sprintf("LODE_TEST_ARG_%d", i)
		env = append(env, name+"="+arg)
		command += ` "$` + name + `"`
	}
	command += ` > "$LODE_TEST_STDOUT"
result=$?
stty -g > "$LODE_TEST_AFTER"
exit "$result"`
	p := exec.CommandContext(ctx, script, "-qefc", command, "/dev/null")
	p.Env = env
	p.WaitDelay = time.Second
	input, err := p.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := p.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.Stderr = p.Stdout
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Process.Kill()
	var transcript bytes.Buffer
	for _, step := range steps {
		var b [1]byte
		for !strings.HasSuffix(transcript.String(), step[0]) {
			if _, err := io.ReadFull(output, b[:]); err != nil {
				t.Fatalf("waiting for %q: %v\n%s", step[0], err, &transcript)
			}
			transcript.WriteByte(b[0])
		}
		if _, err := io.WriteString(input, step[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.Copy(&transcript, output); err != nil {
		t.Fatal(err)
	}
	err = p.Wait()
	if ctx.Err() != nil {
		t.Fatalf("terminal command hung: %v\n%s", ctx.Err(), &transcript)
	}
	before, readErr := os.ReadFile(filepath.Join(root, "before"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	after, readErr := os.ReadFile(filepath.Join(root, "after"))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("terminal settings were not restored: %v", readErr)
	}
	stdout, readErr := os.ReadFile(filepath.Join(root, "stdout"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(stdout), transcript.String(), err
}
