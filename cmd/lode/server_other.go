//go:build !linux

package main

import (
	"context"
	"fmt"
	"io"
)

func runServer(_ context.Context, command string, _ []string, _ string, _ string, _, _ io.Writer) error {
	return fmt.Errorf("%s is a server command and requires Linux", command)
}
