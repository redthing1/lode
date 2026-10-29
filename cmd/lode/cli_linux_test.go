package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/web"
)

// Exercise an installed executable in separate processes, including persisted
// credentials and workspace state. Bootstrap uses the real instance account API.
func TestInstalledCLISelectedWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	t.Setenv("GOBIN", bin)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("LODE_TOKEN", "")
	install := exec.CommandContext(ctx, "go", "install", ".")
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	origin := "http://" + strings.Replace(listener.Addr().String(), "127.0.0.1", "localhost", 1)
	privateOrigin := strings.Replace(origin, "localhost", "private.example", 1)
	dir := filepath.Join(root, "instance")
	code, err := instance.Initialize(ctx, dir, instance.Config{URL: origin, PrivateURL: privateOrigin, Listen: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := instance.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	user, err := store.Setup(ctx, code, "alice", "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(ctx, user, instance.TokenOptions{Name: "cli", Role: instance.Writer})
	if err != nil {
		t.Fatal(err)
	}
	var refuseDownload atomic.Bool
	handler := web.New(store)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuseDownload.Load() && (strings.HasSuffix(r.URL.Path, "/file") || strings.HasSuffix(r.URL.Path, "/files")) {
			http.Error(w, "download interrupted", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	command := func(args ...string) string {
		t.Helper()
		c := exec.CommandContext(ctx, filepath.Join(bin, "lode"), args...)
		c.Dir = root
		output, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("lode %v: %v\n%s", args, err, output)
		}
		if bytes.Contains(output, []byte(token)) {
			t.Fatal("token appeared in command output")
		}
		return string(output)
	}
	help := command("clone", "--help")
	if help != command("help", "clone") || !strings.Contains(help, "--include path") || strings.Contains(help, "  -include") {
		t.Fatal("command help is inconsistent or uses single-dash long options")
	}
	t.Setenv("LODE_TOKEN", token)
	if got := command("login", origin); !strings.Contains(got, "as alice") {
		t.Fatal(got)
	}
	t.Setenv("LODE_TOKEN", "")
	credential, err := credentialPath(origin)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(credential)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions: %v %v", info, err)
	}
	projectURL := origin + "/alice/project"
	if got := command("projects", "--create", "project", origin); strings.TrimSpace(got) != projectURL {
		t.Fatal(got)
	}
	if got := command("projects", origin); !strings.Contains(got, projectURL) {
		t.Fatal(got)
	}
	first := filepath.Join(root, "first")
	if err := os.MkdirAll(filepath.Join(first, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(first, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("opaque-binary\x00\xff"), 160000)
	if err := os.WriteFile(filepath.Join(first, "assets", "model.bin"), large, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "references", "keep"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	command("attach", projectURL, first)
	if got := command("-C", first, "status"); !strings.Contains(got, "A  assets/model.bin") {
		t.Fatal(got)
	}
	if got := command("-C", first, "save", "-m", "initial"); !strings.Contains(got, "Saved ") {
		t.Fatal(got)
	}
	command("-C", first, "tag", "create", "initial")
	// Route the private hostname to this local server without external DNS.
	// The executable still sees a non-loopback HTTP origin and its real Host.
	t.Setenv("HTTP_PROXY", origin)
	t.Setenv("NO_PROXY", "")
	t.Setenv("LODE_TOKEN", token)
	refusedHTTP := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "login", privateOrigin)
	if output, err := refusedHTTP.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("--allow-http")) {
		t.Fatalf("HTTP without consent: %v\n%s", err, output)
	}
	command("login", "--allow-http", privateOrigin)
	t.Setenv("LODE_TOKEN", "")
	privateProjectURL := privateOrigin + "/alice/project"
	if got := command("-C", first, "remote"); strings.TrimSpace(got) != projectURL {
		t.Fatal(got)
	}
	command("-C", first, "remote", "set-url", privateProjectURL)
	if got := command("-C", first, "remote"); strings.TrimSpace(got) != privateProjectURL {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(first, "private-test"), []byte("private connection"), 0644); err != nil {
		t.Fatal(err)
	}
	command("-C", first, "save", "-m", "private save")
	if got := command("projects", privateOrigin); !strings.Contains(got, privateProjectURL) {
		t.Fatal(got)
	}
	// Consent is scoped to this exact origin, even with an environment token.
	t.Setenv("LODE_TOKEN", token)
	otherOrigin := strings.Replace(privateOrigin, "private.example", "other.example", 1)
	otherHTTP := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "projects", otherOrigin)
	if output, err := otherHTTP.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("--allow-http")) {
		t.Fatalf("HTTP consent escaped its origin: %v\n%s", err, output)
	}
	t.Setenv("LODE_TOKEN", "")
	command("-C", first, "remote", "set-url", projectURL)
	second := filepath.Join(root, "second")
	command("clone", "--include", "assets", projectURL, second)
	b, err := os.ReadFile(filepath.Join(second, "assets", "model.bin"))
	if err != nil || !bytes.Equal(b, large) {
		t.Fatalf("clone bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "references")); !os.IsNotExist(err) {
		t.Fatalf("unselected files: %v", err)
	}
	copy(large[1<<20:], bytes.Repeat([]byte{0x42}, 64<<10))
	if err := os.WriteFile(filepath.Join(second, "assets", "model.bin"), large, 0644); err != nil {
		t.Fatal(err)
	}
	command("-C", second, "save", "-m", "selected edit")
	if got := command("-C", second, "log"); !strings.Contains(got, "selected edit") || !strings.Contains(got, "initial") {
		t.Fatal(got)
	}
	third := filepath.Join(root, "third")
	command("clone", projectURL, third)
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || !bytes.Equal(b, large) {
		t.Fatalf("saved bytes: %v", err)
	}
	b, err = os.ReadFile(filepath.Join(third, "references", "keep"))
	if err != nil || string(b) != "keep" {
		t.Fatalf("omitted contents: %q %v", b, err)
	}
	state, err := os.ReadFile(filepath.Join(second, ".lode", "state.json"))
	if err != nil || bytes.Contains(state, []byte(token)) || bytes.Contains(state, []byte("references")) {
		t.Fatalf("workspace scope or credentials: %v", err)
	}
	// The original workspace updates while preserving its independent local work.
	if err := os.WriteFile(filepath.Join(first, "references", "keep"), []byte("local reference edit"), 0644); err != nil {
		t.Fatal(err)
	}
	staleSave := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "-C", first, "save", "-m", "stale save")
	if output, err := staleSave.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("branch changed")) {
		t.Fatalf("stale save: %v\n%s", err, output)
	}
	if err := os.Rename(credential, credential+".saved"); err != nil {
		t.Fatal(err)
	}
	settle := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "-C", first, "cancel")
	if output, err := settle.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("sign in first")) {
		t.Fatalf("publication cancellation without credentials: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(first, ".lode", "save.json")); err != nil {
		t.Fatal("unauthenticated cancellation discarded the pending save", err)
	}
	if err := os.Rename(credential+".saved", credential); err != nil {
		t.Fatal(err)
	}
	command("-C", first, "cancel")
	if _, err := os.Stat(filepath.Join(first, ".lode", "save.json")); !os.IsNotExist(err) {
		t.Fatalf("authenticated cancellation left a pending save: %v", err)
	}
	command("-C", first, "update")
	b, err = os.ReadFile(filepath.Join(first, "assets", "model.bin"))
	if err != nil || !bytes.Equal(b, large) {
		t.Fatalf("updated asset: %v", err)
	}
	if got := command("-C", first, "status"); !strings.Contains(got, "M  references/keep") {
		t.Fatal(got)
	}
	command("-C", first, "save", "-m", "independent edit")
	command("-C", second, "update")
	command("-C", second, "select", "--include", "assets", "--include", "references")
	b, err = os.ReadFile(filepath.Join(second, "references", "keep"))
	if err != nil || string(b) != "local reference edit" {
		t.Fatalf("loaded reference: %q %v", b, err)
	}
	command("-C", second, "select", "--include", "assets")
	if _, err := os.Stat(filepath.Join(second, "references")); !os.IsNotExist(err) {
		t.Fatalf("unloaded reference: %v", err)
	}
	if got := command("-C", second, "select"); strings.TrimSpace(got) != "include assets" {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(first, "assets", "model.bin"), []byte("incoming conflict"), 0644); err != nil {
		t.Fatal(err)
	}
	command("-C", first, "save", "-m", "incoming conflict")
	if err := os.WriteFile(filepath.Join(third, "assets", "model.bin"), []byte("local conflict"), 0644); err != nil {
		t.Fatal(err)
	}
	conflict := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "-C", third, "update")
	output, conflictErr := conflict.CombinedOutput()
	if conflictErr == nil || !bytes.Contains(output, []byte("C  assets/model.bin")) || !bytes.Contains(output, []byte("--incoming")) {
		t.Fatalf("conflict presentation: %v\n%s", conflictErr, output)
	}
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || string(b) != "local conflict" {
		t.Fatalf("conflict preservation: %q %v", b, err)
	}
	command("-C", third, "update", "--incoming", "assets/model.bin")
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || string(b) != "incoming conflict" {
		t.Fatalf("conflict choice: %q %v", b, err)
	}
	command("-C", third, "branch", "create", "experiment")
	if got := command("-C", third, "branch"); !strings.Contains(got, "experiment") || !strings.Contains(got, "* main") {
		t.Fatal(got)
	}
	command("-C", third, "switch", "experiment")
	if err := os.WriteFile(filepath.Join(third, "assets", "model.bin"), []byte("branch contents"), 0644); err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(command("-C", third, "save", "-m", "experiment"), "Saved "))
	command("-C", third, "switch", "main")
	command("-C", third, "restore", model.ShortVersion(version), "assets/model.bin")
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || string(b) != "branch contents" {
		t.Fatalf("CLI restore: %q %v", b, err)
	}
	if got := command("-C", third, "status"); !strings.Contains(got, "· main") || !strings.Contains(got, "M  assets/model.bin") {
		t.Fatal(got)
	}
	fourth := filepath.Join(root, "fourth")
	command("clone", "--branch", "experiment", "--include", "assets", projectURL, fourth)
	b, err = os.ReadFile(filepath.Join(fourth, "assets", "model.bin"))
	if err != nil || string(b) != "branch contents" {
		t.Fatalf("CLI branch clone: %q %v", b, err)
	}
	if got := command("-C", fourth, "log"); !strings.Contains(got, model.ShortVersion(version)) {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(third, "assets", "model.bin"), []byte("target branch change"), 0644); err != nil {
		t.Fatal(err)
	}
	command("-C", third, "save", "-m", "target change")
	conflict = exec.CommandContext(ctx, filepath.Join(bin, "lode"), "-C", third, "merge", "experiment")
	output, conflictErr = conflict.CombinedOutput()
	if conflictErr == nil || !bytes.Contains(output, []byte("C  assets/model.bin")) || !bytes.Contains(output, []byte("--incoming")) {
		t.Fatalf("merge conflict presentation: %v\n%s", conflictErr, output)
	}
	if got := command("-C", third, "merge", "--incoming", "assets/model.bin", "experiment"); !strings.Contains(got, "Merged ") {
		t.Fatal(got)
	}
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || string(b) != "branch contents" {
		t.Fatalf("CLI merge: %q %v", b, err)
	}
	if got := command("-C", third, "status"); !strings.Contains(got, "No selected changes.") {
		t.Fatal(got)
	}
	if got := command("-C", third, "tag"); !strings.Contains(got, "initial") {
		t.Fatal(got)
	}
	maintenanceToken, err := store.CreateToken(ctx, user, instance.TokenOptions{Name: "maintenance", Role: instance.Maintainer})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LODE_TOKEN", maintenanceToken)
	command("-C", third, "retention", "--keep", "1", "--history-days", "1")
	preview := command("-C", third, "cleanup")
	if !strings.Contains(preview, "Expire ") || !strings.Contains(preview, "Local files are unchanged.") {
		t.Fatalf("cleanup preview: %s", preview)
	}
	_, apply, ok := strings.Cut(preview, "lode cleanup --apply ")
	if !ok {
		t.Fatal("missing apply command")
	}
	if got := command("-C", third, "cleanup", "--apply", strings.TrimSpace(apply)); !strings.Contains(got, "Cleanup applied;") {
		t.Fatal(got)
	}
	if got := command("-C", third, "collect"); !strings.Contains(got, "Content repository:") || !strings.Contains(got, "allocated.") {
		t.Fatal(got)
	}
	if got := command("-C", third, "log"); !strings.Contains(got, "contents expired") {
		t.Fatal(got)
	}
	if got := command("-C", third, "status"); !strings.Contains(got, "No selected changes.") {
		t.Fatal(got)
	}
	command("-C", third, "restore", "tag:initial", "assets/model.bin")
	b, err = os.ReadFile(filepath.Join(third, "assets", "model.bin"))
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte("opaque-binary\x00\xff"), 160000)) {
		t.Fatalf("protected restore after cleanup: %v", err)
	}
	command("-C", third, "tag", "delete", "initial")
	if got := command("-C", third, "tag"); strings.Contains(got, "initial") {
		t.Fatal("deleted tag still listed")
	}
	t.Setenv("LODE_TOKEN", "")
	newPassword := "a replacement password for this test"
	if err := store.ChangePassword(ctx, instance.Principal{User: user}, "a sufficiently long test password", newPassword); err != nil {
		t.Fatal(err)
	}
	refused := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "projects", origin)
	output, err = refused.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("invalid credentials")) || bytes.Contains(output, []byte(token)) {
		t.Fatalf("revoked saved credential: %v\n%s", err, output)
	}
	if got := command("-C", second, "status"); !strings.Contains(got, "No selected changes.") {
		t.Fatal(got)
	}
	user, err = store.Password(ctx, "alice", newPassword)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.CreateToken(ctx, user, instance.TokenOptions{Name: "replacement", Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LODE_TOKEN", replacement)
	command("login", origin)
	t.Setenv("LODE_TOKEN", "")
	if got := command("projects", origin); !strings.Contains(got, projectURL) {
		t.Fatal(got)
	}
	lockedRecovery := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "recover-user", dir, "alice")
	lockedRecovery.Stdin = strings.NewReader("operator recovery password\n")
	if output, err := lockedRecovery.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("already in use")) {
		t.Fatalf("recovery while server owns instance: %v\n%s", err, output)
	}
	// Cancellation of local installation must not depend on saved credentials.
	// Leave real clone plans behind by refusing their file downloads.
	refuseDownload.Store(true)
	var interrupted []string
	for _, name := range []string{"missing-credentials", "invalid-credentials"} {
		destination := filepath.Join(root, name)
		clone := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "clone", "--include", "assets/model.bin", projectURL, destination)
		if output, err := clone.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("download interrupted")) {
			t.Fatalf("interrupted clone: %v\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(destination, ".lode", "transition.json")); err != nil {
			t.Fatal("clone did not preserve its local operation", err)
		}
		interrupted = append(interrupted, destination)
	}
	server.Close()
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	for i, destination := range interrupted {
		if i == 1 {
			if err := os.WriteFile(credential, []byte("invalid saved credentials"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		command("-C", destination, "cancel")
		if _, err := os.Stat(filepath.Join(destination, ".lode", "transition.json")); !os.IsNotExist(err) {
			t.Fatalf("cancel left a transition: %v", err)
		}
		if _, err := os.Stat(filepath.Join(destination, "assets", "model.bin")); !os.IsNotExist(err) {
			t.Fatalf("cancel left a downloaded file: %v", err)
		}
		command("-C", destination, "cancel")
	}
	if got := command("-C", second, "status"); !strings.Contains(got, "No selected changes.") {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(second, "assets", "new"), []byte("offline"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := command("-C", filepath.Join(second, "assets"), "status"); !strings.Contains(got, "A  assets/new") {
		t.Fatal(got)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	operatorPassword := "operator recovery password"
	recoverCommand := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "recover-user", dir, "alice")
	recoverCommand.Stdin = strings.NewReader(operatorPassword + "\n")
	output, err = recoverCommand.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("Account recovered")) || bytes.Contains(output, []byte(operatorPassword)) {
		t.Fatalf("operator recovery: %v\n%s", err, output)
	}
	reopened, err := instance.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	recovered, err := reopened.Password(ctx, "alice", operatorPassword)
	if err != nil || recovered.ID != user.ID || !recovered.Admin {
		t.Fatalf("recovered account: %+v %v", recovered, err)
	}
	if _, err := reopened.Token(ctx, replacement); err == nil {
		t.Fatal("operator recovery retained old token")
	}
	if _, err := reopened.Project(ctx, instance.Principal{User: recovered}, "alice", "project", instance.Maintainer); err != nil {
		t.Fatal("operator recovery lost owned project", err)
	}
	backup := filepath.Join(root, "backup")
	lockedBackup := exec.CommandContext(ctx, filepath.Join(bin, "lode"), "backup", dir, backup)
	if output, err := lockedBackup.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("stop the server")) {
		t.Fatalf("backup while server owns instance: %v\n%s", err, output)
	}
	if err := reopened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := command("-C", root, "backup", "instance", "backup"); !strings.Contains(got, "Backup verified:") {
		t.Fatal(got)
	}
	command("-C", root, "backup", "backup", "restored")
	restored, err := instance.Open(ctx, filepath.Join(root, "restored"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	if got, err := restored.Password(ctx, "alice", operatorPassword); err != nil || got != recovered || restored.ID != store.ID {
		t.Fatal("CLI restore changed account or instance identity", err)
	}
}
