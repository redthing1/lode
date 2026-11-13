package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

func readInputLine(ctx context.Context, in io.Reader, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(in, limit)).ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func promptSecrets(ctx context.Context, prompts ...string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer term.Restore(fd, state)
	tty := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}, "")
	type result struct {
		values []string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var values []string
		for _, prompt := range prompts {
			if err := ctx.Err(); err != nil {
				done <- result{err: err}
				return
			}
			value, err := tty.ReadPassword(prompt)
			if err != nil {
				done <- result{err: err}
				return
			}
			values = append(values, value)
		}
		done <- result{values: values}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprint(os.Stderr, "\r\n")
			if errors.Is(r.err, io.EOF) {
				r.err = errors.New("input cancelled")
			}
		}
		return r.values, r.err
	case <-ctx.Done():
		fmt.Fprint(os.Stderr, "\r\n")
		return nil, ctx.Err()
	}
}
