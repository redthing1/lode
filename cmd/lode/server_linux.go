package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/web"
)

func runServer(ctx context.Context, command string, args []string, address, publicURL string, out, diagnostics io.Writer) error {
	if command == "backup" {
		if len(args) != 2 {
			return errors.New("usage: lode backup SOURCE DEST")
		}
		if err := instance.Backup(ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Backup created: %s\n", args[1])
		return nil
	}
	if command == "import" || command == "recover" {
		count := 3
		if command == "recover" {
			count = 2
		}
		if len(args) != count {
			return fmt.Errorf("use lode help %s for syntax", command)
		}
		i, err := instance.Open(ctx, args[0])
		if err != nil {
			return err
		}
		defer i.Close()
		if command == "import" {
			p, err := i.ImportProject(ctx, args[1], args[2])
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Imported project: %s/p/%s\n", i.Config.URL, p.Slug)
			return nil
		}
		password := project.NewID()
		if _, err := i.ResetAccount(ctx, args[1], password); err != nil {
			return err
		}
		fmt.Fprintf(out, "Temporary password for %s: %s\nSign in and change it. Existing sessions, keys and tokens were revoked.\n", args[1], password)
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: lode %s [options] DIR", command)
	}
	if command == "serve" {
		return serve(ctx, args[0], address, diagnostics)
	}
	i, code, err := instance.Create(ctx, args[0], publicURL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Instance: %s\nSetup: %s/setup\nSetup code: %s\nRun lode serve %s, then open the setup page.\n", i.Config.ID, i.Config.URL, code, args[0])
	return i.Close()
}

func serve(ctx context.Context, directory, address string, out io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("serve requires a loopback address; use a TLS reverse proxy for remote access")
	}
	i, err := instance.Open(ctx, directory)
	if err != nil {
		return err
	}
	defer i.Close()
	handler, err := web.New(i, buildVersion())
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	fmt.Fprintf(out, "Serving %s on %s\n", i.Config.URL, listener.Addr())
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
