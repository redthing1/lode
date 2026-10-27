package workspace

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// withReadHold renews a bounded server hold while a workspace materializes an
// immutable source. A failed operation keeps its durable pending hold until
// expiry so Resume can continue with the same ID.
func (w *Workspace) withReadHold(ctx context.Context, id, commitID string, fn func(context.Context) error) error {
	lease, err := w.client.HoldRead(ctx, id, commitID)
	if err != nil {
		return err
	}
	interval := lease.Lifetime / 3
	if interval <= 0 {
		w.releaseReadBestEffort(id)
		return errors.New("remote returned an invalid read hold lifetime")
	}
	renewCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				if _, err := w.client.HoldRead(renewCtx, id, commitID); err != nil {
					if renewCtx.Err() != nil {
						renewDone <- nil
						return
					}
					cancel()
					renewDone <- err
					return
				}
			}
		}
	}()

	operationErr := fn(renewCtx)
	cancel()
	renewErr := <-renewDone
	pending := w.hasPendingReadHold(id)
	if renewErr != nil && !(operationErr == nil && !pending) {
		operationErr = errors.Join(operationErr, fmt.Errorf("renew read hold: %w", renewErr))
	}
	if operationErr == nil || !pending {
		w.releaseReadBestEffort(id)
	}
	return operationErr
}

func (w *Workspace) hasPendingReadHold(id string) bool {
	if w.state.Pending == nil {
		return false
	}
	switch w.state.Pending.Kind {
	case "checkout":
		return w.state.Pending.ReadID == id
	case "transition":
		return w.state.Pending.Transition != nil && w.state.Pending.Transition.ID == id
	default:
		return false
	}
}

func (w *Workspace) releaseReadBestEffort(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.client.ReleaseRead(ctx, id)
}

func validReadID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}
