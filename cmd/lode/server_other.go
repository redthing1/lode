//go:build !linux

package main

import (
	"context"
	"errors"
	"io"
)

func serverCommand(context.Context, string, []string, string, io.Writer) error {
	return errors.New("server commands require Linux")
}
