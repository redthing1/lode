package content

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	kcontent "github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/content/indexblob"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotgc"
	"github.com/redthing1/lode/internal/model"
)

// Usage requires exclusion of writes for a consistent measurement. File sizes
// and allocated blocks measure different things; neither includes the cache.
func (s *Store) Usage(ctx context.Context) (model.StorageUsage, error) {
	var usage model.StorageUsage
	err := filepath.WalkDir(s.directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			usage.Bytes += info.Size()
		}
		usage.Allocated += info.Sys().(*syscall.Stat_t).Blocks * 512
		return nil
	})
	return usage, err
}

// Collect requires exclusive use of this local, strongly consistent repository:
// no open readers/writers and no other Kopia clients. The project service owns
// that gate and has already reconciled all promised snapshot protections.
func (s *Store) Collect(ctx context.Context) (_ model.Collection, err error) {
	var result model.Collection
	result.Before, err = s.Usage(ctx)
	if err != nil {
		return result, err
	}
	direct := s.repository.(repo.DirectRepository)
	defer func() {
		// Maintenance can persist partial work before cancellation or failure.
		// Refresh the shared index even when the request context was cancelled.
		err = errors.Join(err, s.repository.Refresh(context.WithoutCancel(ctx)))
	}()
	err = repo.DirectWriteSession(ctx, direct, repo.WriteSessionOptions{Purpose: "collection owner"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		params, err := maintenance.GetParams(ctx, w)
		if err != nil {
			return err
		}
		owner := w.ClientOptions().UsernameAtHost()
		if params.Owner == owner {
			return nil
		}
		params.Owner = owner
		return maintenance.SetParams(ctx, w, params)
	})
	if err != nil {
		return result, err
	}
	completed := false
	err = repo.DirectWriteSession(ctx, direct, repo.WriteSessionOptions{Purpose: "collect expired contents"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		if err := w.Refresh(ctx); err != nil {
			return err
		}
		latest, err := settleContents(ctx, w)
		if err != nil {
			return err
		}
		return maintenance.RunExclusive(ctx, w, maintenance.ModeFull, false, func(ctx context.Context, run maintenance.RunParameters) error {
			// The filesystem's maintenance-marker timestamp may lag the wall
			// clock. These contents have already settled under our exclusive
			// gate, so include them even when the marker is slightly older.
			cutoff := run.MaintenanceStartTime
			if latest.After(cutoff) {
				cutoff = latest
			}
			if err := snapshotgc.Run(ctx, w, true, maintenance.SafetyNone, cutoff); err != nil {
				return err
			}
			// Native timestamps are monotonic whole seconds. A new deletion can
			// be in the future; ordinary full maintenance can rewrite it before
			// dropping it, advancing its timestamp again. Drop settled deletions
			// first so obsolete bytes do not enter the compaction work.
			if _, err := settleContents(ctx, w); err != nil {
				return err
			}
			if _, err := w.ContentManager().CompactIndexes(ctx, indexblob.CompactOptions{AllIndexes: true, DropDeletedBefore: w.Time(), DisableEventualConsistencySafety: true}); err != nil {
				return err
			}
			// Native short-pack consolidation skips a lone pack. Rewrite live
			// contents when at least half the stored pack and 1 MiB are wasted.
			ids, err := sparseContents(ctx, w)
			if err != nil {
				return err
			}
			if len(ids) != 0 {
				if _, err := maintenance.RewriteContents(ctx, w, &maintenance.RewriteContentsOptions{ContentIDs: ids, Parallel: 2}, maintenance.SafetyNone); err != nil {
					return err
				}
			}
			err = maintenance.Run(ctx, run, maintenance.SafetyNone)
			completed = err == nil
			return err
		})
	})
	if err != nil {
		return result, err
	}
	if !completed {
		return result, errors.New("storage maintenance is already running; retry collection")
	}
	result.After, err = s.Usage(ctx)
	return result, err
}

func settleContents(ctx context.Context, r repo.DirectRepository) (time.Time, error) {
	var latest time.Time
	if err := r.ContentReader().IterateContents(ctx, kcontent.IterateOptions{IncludeDeleted: true}, func(i kcontent.Info) error {
		if i.Timestamp().After(latest) {
			latest = i.Timestamp()
		}
		return nil
	}); err != nil {
		return time.Time{}, err
	}
	for !r.Time().After(latest) {
		remaining := latest.Sub(r.Time())
		// Match native maintenance's clock-skew limit instead of holding the
		// project indefinitely after a substantial server clock rollback.
		if remaining > 5*time.Minute {
			return time.Time{}, fmt.Errorf("content timestamp is %s ahead of the server clock; correct the clock before collecting", remaining.Round(time.Second))
		}
		timer := time.NewTimer(remaining + time.Nanosecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return time.Time{}, ctx.Err()
		case <-timer.C:
		}
	}
	return latest, ctx.Err()
}

func sparseContents(ctx context.Context, r repo.DirectRepositoryWriter) ([]kcontent.ID, error) {
	live := map[blob.ID]int64{}
	err := r.ContentReader().IterateContents(ctx, kcontent.IterateOptions{}, func(i kcontent.Info) error {
		if !i.Deleted && (strings.HasPrefix(string(i.PackBlobID), string(kcontent.PackBlobIDPrefixRegular)) || strings.HasPrefix(string(i.PackBlobID), string(kcontent.PackBlobIDPrefixSpecial))) {
			live[i.PackBlobID] += int64(i.PackedLength)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	selected := map[blob.ID]bool{}
	for pack, size := range live {
		metadata, err := r.BlobStorage().GetMetadata(ctx, pack)
		if err != nil {
			return nil, err
		}
		if size*2 <= metadata.Length && metadata.Length-size >= 1<<20 {
			selected[pack] = true
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	var ids []kcontent.ID
	err = r.ContentReader().IterateContents(ctx, kcontent.IterateOptions{}, func(i kcontent.Info) error {
		if !i.Deleted && selected[i.PackBlobID] {
			ids = append(ids, i.ContentID)
		}
		return nil
	})
	return ids, err
}
