package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/web"
	"golang.org/x/term"
)

func serverCommand(ctx context.Context, command string, args []string, directory string, out io.Writer) error {
	usage := "Usage: lode " + command + " [options] <instance-directory>"
	arguments := 1
	if command == "recover-user" {
		usage += " <username>\nRead a new password from the terminal or one line on stdin."
		arguments = 2
	}
	if command == "backup" {
		usage = "Usage: lode backup <source-instance> <new-destination>\nThe source must be stopped. Restore uses the same command."
		arguments = 2
	}
	f := flags(command, usage, out)
	var origin, listen string
	if command == "init" {
		f.StringVar(&origin, "url", "http://localhost:8473", "canonical server origin")
		f.StringVar(&listen, "listen", "127.0.0.1:8473", "HTTP listen address")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != arguments {
		return errors.New(usage)
	}
	dir := f.Arg(0)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(directory, dir)
	}
	if command == "init" {
		code, err := instance.Initialize(ctx, dir, instance.Config{URL: origin, Listen: listen})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Instance created.\nSetup code: %s\nRun lode serve %s\n", code, dir)
		return nil
	}
	if command == "backup" {
		destination := f.Arg(1)
		if !filepath.IsAbs(destination) {
			destination = filepath.Join(directory, destination)
		}
		if err := instance.Backup(ctx, dir, destination); err != nil {
			return err
		}
		fmt.Fprintf(out, "Backup verified: %s\n", destination)
		return nil
	}
	store, err := instance.Open(ctx, dir)
	if err != nil {
		return err
	}
	defer store.Close(context.Background())
	if command == "recover-user" {
		password, err := recoveryPassword(out)
		if err != nil {
			return err
		}
		if err := store.RecoverAccount(ctx, f.Arg(1), password); err != nil {
			return err
		}
		fmt.Fprintln(out, "Account recovered. Existing sessions, tokens, keys and recovery codes are invalid.")
		return nil
	}
	setup, err := store.NeedsSetup(ctx)
	if err != nil {
		return err
	}
	if setup {
		code, err := store.SetupCode(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Administrator setup: %s/setup\nSetup code: %s\n", store.Config.URL, code)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: store.Config.Listen, Handler: web.New(store), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	fmt.Fprintf(out, "Lode at %s\n", store.Config.URL)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			<-done
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func recoveryPassword(out io.Writer) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 75)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
	}
	fmt.Fprint(out, "New password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	fmt.Fprint(out, "Confirm password: ")
	confirmation, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	if string(password) != string(confirmation) {
		return "", errors.New("passwords do not match")
	}
	return string(password), nil
}
