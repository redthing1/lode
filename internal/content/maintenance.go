package content

import (
	"context"
	"fmt"
	"math"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

// Collection reports physical bytes in the native repository before and after
// a full native maintenance pass. It excludes the Kopia config and caches.
type Collection struct {
	BeforeBytes int64 `json:"before_bytes"`
	AfterBytes  int64 `json:"after_bytes"`
}

// Collect runs Kopia's full snapshot GC and repository maintenance. Lode's
// project gate must exclude concurrent project operations.
func (s *Store) Collect(ctx context.Context) (Collection, error) {
	var result Collection
	dw, ok := s.repository.(repo.DirectRepositoryWriter)
	if !ok {
		return result, fmt.Errorf("content repository does not support native maintenance")
	}

	before, err := physicalBlobBytes(ctx, dw.BlobStorage())
	if err != nil {
		return result, fmt.Errorf("measure repository before collection: %w", err)
	}
	result.BeforeBytes = before

	params, err := maintenance.GetParams(ctx, dw)
	if err != nil {
		return result, fmt.Errorf("read native maintenance parameters: %w", err)
	}
	if params.Owner == "" {
		params.Owner = dw.ClientOptions().UsernameAtHost()
		if err := repo.DirectWriteSession(ctx, dw, repo.WriteSessionOptions{Purpose: "lode initialize maintenance owner"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
			return maintenance.SetParams(ctx, writer, params)
		}); err != nil {
			return result, fmt.Errorf("initialize native maintenance owner: %w", err)
		}
	}

	if err := snapshotmaintenance.Run(ctx, dw, maintenance.ModeFull, false, maintenance.SafetyFull); err != nil {
		return result, fmt.Errorf("run native full maintenance: %w", err)
	}
	after, err := physicalBlobBytes(ctx, dw.BlobStorage())
	if err != nil {
		return result, fmt.Errorf("measure repository after collection: %w", err)
	}
	result.AfterBytes = after
	return result, nil
}

func physicalBlobBytes(ctx context.Context, storage blob.Storage) (int64, error) {
	var total int64
	err := storage.ListBlobs(ctx, "", func(b blob.Metadata) error {
		if b.Length < 0 || total > math.MaxInt64-b.Length {
			return fmt.Errorf("repository blob byte total overflows int64")
		}
		total += b.Length
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
