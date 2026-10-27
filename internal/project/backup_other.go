//go:build !linux

package project

import (
	"context"
)

// BackupLocal is unavailable on platforms without the local project durability
// primitives used by the Linux implementation.
func BackupLocal(context.Context, string, string) error {
	return errLocalUnsupported
}
