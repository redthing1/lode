package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
	"github.com/redthing1/lode/internal/workspace"
)

func TestRunHelpVersionAndUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(nil, &stdout, &stderr, "test-build"); code != 0 || !strings.Contains(stdout.String(), "Commands:") {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"version"}, &stdout, &stderr, "test-build"); code != 0 || stdout.String() != "lode test-build\n" {
		t.Fatalf("version: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"future-command"}, &stdout, &stderr, "test-build"); code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unknown command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestHelpListsWorkspaceAndBranchCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help"}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("help code=%d stderr=%q", code, stderr.String())
	}
	for _, command := range []string{"update [-C DIR]", "switch [-C DIR] BRANCH", "load [-C DIR] PATH...", "unload [-C DIR] PATH...", "restore [-C DIR] --from COMMIT PATH...", "merge [-C DIR]", "branch create", "branch delete", "branch list", "--ours PATH", "--theirs PATH"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help missing %q", command)
		}
	}
}

func TestMergeCommandPreviewAndChoiceAgainstLocalServer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := project.InitializeLocal(ctx, store); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	directory := filepath.Join(root, "workspace")
	w, err := workspace.Create(ctx, workspace.Options{Directory: directory, Remote: server.URL, Token: local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "shared.txt")
	if err := os.WriteFile(file, []byte("base"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	base, err := w.Commit(ctx, "cli-test", "base")
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.CreateBranch(ctx, "topic", base.ID); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("main version"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, "cli-test", "main change"); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Switch(ctx, "topic"); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("topic version"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, "cli-test", "topic change"); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Switch(ctx, "main"); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	invoke := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run(append([]string{args[0], "-C", directory}, args[1:]...), &stdout, &stderr, "test")
		if code != 0 {
			return stdout.String(), errors.New(stderr.String())
		}
		return stdout.String(), nil
	}
	preview, err := invoke("merge", "--preview", "topic")
	if err != nil || !strings.Contains(preview, "conflicts: 1") || !strings.Contains(preview, `"shared.txt"`) {
		t.Fatalf("merge preview = %q, %v", preview, err)
	}
	mergedOutput, err := invoke("merge", "--theirs", "shared.txt", "--author", "cli-test", "-m", "merge topic", "topic")
	if err != nil || !strings.Contains(mergedOutput, "merged") {
		t.Fatalf("merge command = %q, %v", mergedOutput, err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "topic version" {
		t.Fatalf("selected merge file = %q, %v", got, err)
	}
}

func TestChoiceFlagsValidateDuplicatesBeforeOpeningWorkspace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"merge", "--preview", "--ours", "file", "--theirs", "file", "topic"}, &stdout, &stderr, "test")
	if code != 1 || !strings.Contains(stderr.String(), "duplicate choice") {
		t.Fatalf("duplicate choice flags: code=%d stderr=%q", code, stderr.String())
	}
	choices, err := makeChoices(stringList{".", "dir/file"}, stringList{"dir"})
	if err != nil || len(choices) != 3 || choices[0].Path != "" || choices[0].Side != model.LocalSide || choices[2].Side != model.TargetSide {
		t.Fatalf("makeChoices() = %+v, %v", choices, err)
	}
}

func TestAbortAndResumeReportSafeMaterializationAbandonment(t *testing.T) {
	directory := createIncompleteCLIWorkspace(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"abort", "-C", directory}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("abort: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stopped materialization; current files kept; recovery data: .lode/recovery-") {
		t.Fatalf("abort output=%q", stdout.String())
	}
	if got, err := os.ReadFile(filepath.Join(directory, "a.bin")); err != nil || string(got) != "a" {
		t.Fatalf("checkout file after abort=%q err=%v", got, err)
	}

	directory = createIncompleteCLIWorkspace(t)
	workspaceHandle, err := workspace.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(directory, ".lode", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stateFile struct {
		Pending struct {
			ReadID string
		} `json:"Pending"`
	}
	if err := json.Unmarshal(stateBytes, &stateFile); err != nil {
		t.Fatal(err)
	}
	readID := stateFile.Pending.ReadID
	foreign := filepath.Join(directory, recoveryDirForCLI(readID))
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "journal.json"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceHandle.Abort(context.Background()); err == nil {
		t.Fatal("Abort unexpectedly adopted a foreign recovery archive")
	}
	if err := workspaceHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(foreign); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"resume", "-C", directory}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("resume abandonment: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stopped materialization; current files kept; recovery data: "+recoveryDirForCLI(readID)) {
		t.Fatalf("resume output=%q", stdout.String())
	}
}

func createIncompleteCLIWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "store")
	if err := project.InitializeLocal(ctx, store); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	var failSecond atomic.Bool
	failSecond.Store(true)
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/file" && r.URL.Query().Get("path") == "b.bin" && failSecond.CompareAndSwap(true, false) {
			http.Error(w, "interrupted checkout", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { server.Close(); _ = local.Close() })
	contents := map[string][]byte{"a.bin": []byte("a"), "b.bin": []byte("b")}
	changes := make([]project.Change, 0, len(contents))
	for name, data := range contents {
		digest := sha256.Sum256(data)
		changes = append(changes, project.Change{Path: name, Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))})
	}
	_, err = local.Service.Apply(ctx, project.ChangeRequest{OperationID: "cli-abandon-seed", Branch: "main", Author: "test", Message: "seed", Changes: changes}, func(path model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents[path.String()])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "workspace")
	if _, err := workspace.Create(ctx, workspace.Options{Directory: directory, Remote: server.URL, Token: local.Token, Branch: "main"}); err == nil {
		t.Fatal("checkout unexpectedly completed")
	}
	return directory
}

func recoveryDirForCLI(id string) string { return ".lode/recovery-" + id }

func TestNewCommandArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		{"update", "extra"}, {"switch"}, {"switch", "a", "b"},
		{"load"}, {"unload"}, {"restore", "file"}, {"restore", "--from", "commit"},
		{"branch"}, {"branch", "create"}, {"branch", "delete"},
		{"branch", "list", "extra"}, {"branch", "unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test"); code != 1 || stdout.Len() != 0 {
			t.Errorf("Run(%q) = code %d stdout=%q stderr=%q; want argument error", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestBranchAndTransitionCommandsAgainstLocalServer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := project.InitializeLocal(ctx, store); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte(local.Token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(root, "workspace")
	w, err := workspace.Create(ctx, workspace.Options{Directory: workspaceDir, Remote: server.URL, Token: local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspaceDir, "file.bin"), []byte("first"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	first, err := w.Commit(ctx, "cli-test", "first tree")
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	invoke := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		commandArgs := []string{args[0]}
		if args[0] == "branch" && len(args) > 1 {
			commandArgs = append(commandArgs, args[1], "-C", workspaceDir)
			commandArgs = append(commandArgs, args[2:]...)
		} else {
			commandArgs = append(commandArgs, "-C", workspaceDir)
			commandArgs = append(commandArgs, args[1:]...)
		}
		code := Run(commandArgs, &stdout, &stderr, "test")
		if code != 0 {
			return stdout.String(), errors.New(stderr.String())
		}
		return stdout.String(), nil
	}
	if _, err = invoke("branch", "create", "topic"); err != nil {
		t.Fatalf("branch create: %v", err)
	}
	listed, err := invoke("branch", "list")
	if err != nil || !strings.Contains(listed, "main "+first.ID) || !strings.Contains(listed, "topic "+first.ID) {
		t.Fatalf("branch list=%q err=%v", listed, err)
	}

	w, err = workspace.Open(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspaceDir, "file.bin"), []byte("second"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	second, err := w.Commit(ctx, "cli-test", "second tree")
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = invoke("switch", "topic"); err != nil {
		t.Fatalf("switch topic: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspaceDir, "file.bin")); err != nil || string(got) != "first" {
		t.Fatalf("topic file=%q err=%v", got, err)
	}
	if _, err = invoke("switch", "main"); err != nil {
		t.Fatalf("switch main: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspaceDir, "file.bin")); err != nil || string(got) != "second" {
		t.Fatalf("main file=%q err=%v", got, err)
	}
	updated, err := invoke("update")
	if err != nil || !strings.Contains(updated, second.ID) || !strings.Contains(updated, "main") {
		t.Fatalf("update output=%q err=%v", updated, err)
	}
	if _, err = invoke("branch", "delete", "topic"); err != nil {
		t.Fatalf("branch delete: %v", err)
	}
}

func TestHandlerGateWaitsForActiveHandlersAndRejectsLateRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	gate := &handlerGate{next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	firstResponse := httptest.NewRecorder()
	firstRequestDone := make(chan struct{})
	go func() {
		gate.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodGet, "/", nil))
		close(firstRequestDone)
	}()
	<-started

	closed := make(chan struct{})
	go func() {
		gate.CloseAndWait()
		close(closed)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		isClosing := gate.closing
		gate.mu.Unlock()
		if isClosing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not close handler admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		t.Fatal("CloseAndWait returned while a handler was active")
	case <-time.After(10 * time.Millisecond):
	}

	lateResponse := httptest.NewRecorder()
	gate.ServeHTTP(lateResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	if lateResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("late request status = %d, want 503", lateResponse.Code)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not finish after the active handler returned")
	}
	<-firstRequestDone
	if firstResponse.Code != http.StatusNoContent {
		t.Fatalf("active request status = %d, want 204", firstResponse.Code)
	}
}

func TestCommandHelpPrintsRegisteredFlags(t *testing.T) {
	var output bytes.Buffer
	fs := newFlags("clone [options] URL FOLDER", &output)
	fs.String("token-file", "", "private token file")
	fs.String("branch", "main", "branch name")
	err := fs.Parse([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "-token-file") || !strings.Contains(output.String(), "-branch") {
		t.Fatalf("help error=%v output=%q", err, output.String())
	}
}

func TestLoopbackAddressRestrictsCleartextBinding(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"127.0.0.1:8021", "127.0.0.1:8021"},
		{"localhost:0", "127.0.0.1:0"},
		{"[::1]:8021", "[::1]:8021"},
	} {
		got, err := loopbackAddress(context.Background(), test.input)
		if err != nil || got != test.want {
			t.Errorf("loopbackAddress(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"0.0.0.0:8021", "192.0.2.1:8021", "example.com:8021", "127.0.0.1:nope", "127.0.0.1:65536"} {
		if _, err := loopbackAddress(context.Background(), input); err == nil {
			t.Errorf("loopbackAddress(%q) unexpectedly accepted", input)
		}
	}
}

func TestReadTokenBoundedAndNeverEchoed(t *testing.T) {
	dir := t.TempDir()
	secret := "sensitive-token-value"
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readToken(tokenPath)
	if err != nil || got != secret {
		t.Fatalf("readToken() = %q, %v", got, err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"clone", "--token-file", tokenPath, "http://example.com", filepath.Join(dir, "folder")}, &stdout, &stderr, "dev")
	if code == 0 || strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatalf("failed clone leaked token or succeeded: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	if err := os.WriteFile(tokenPath, bytes.Repeat([]byte("x"), tokenFileLimit+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(tokenPath); err == nil {
		t.Fatal("oversized token file was accepted")
	}
}

func TestServeAddressActuallyBindsLoopback(t *testing.T) {
	address, err := loopbackAddress(context.Background(), "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ip, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || !net.ParseIP(ip).IsLoopback() {
		t.Fatalf("listener bound a non-loopback address: %q, %v", listener.Addr(), err)
	}
}
