package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

func runServer(ctx context.Context, command string, args []string, address string, out, diagnostics io.Writer) error {
	if command == "backup" {
		if len(args) != 2 {
			return errors.New("usage: lode backup SOURCE DEST")
		}
		if err := project.Backup(ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Backup created: %s\n", args[1])
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: lode %s [options] DIR", command)
	}
	if command == "serve" {
		return serve(ctx, args[0], address, diagnostics)
	}
	p, err := project.Create(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Project: %s\nToken: %s\n", p.ID(), p.Token())
	return p.Close()
}

func serve(ctx context.Context, directory, address string, out io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("serve requires a loopback address; use a TLS reverse proxy for remote access")
	}
	p, err := project.Open(ctx, directory)
	if err != nil {
		return err
	}
	defer p.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: remote.Handler(p), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	fmt.Fprintf(out, "Serving on %s\n", listener.Addr())
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
