package project

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/redthing1/lode/internal/engine/kopia"
)

// ContentMaintenance is the native IO boundary for capacity and collection.
// The service owns admission, serialization, and durable compaction checkpoints.
// Substitution is useful for capacity and process-interruption tests.
type ContentMaintenance interface {
	Namespace() string
	Inspect(context.Context) (kopia.RepositoryStats, error)
	Capacity(context.Context) (blob.Capacity, error)
	RunCollection(context.Context, kopia.CollectionOptions) (kopia.CollectionReport, error)
}

const DefaultRewriteBytes uint64 = 64 << 20

type CollectRequest struct {
	MaxRewriteBytes uint64
}

// CollectionResult keeps measured blob lengths separate from indexed bytes
// rewritten. Native safety can require several invocations separated by time.
type CollectionResult struct {
	Before, After                  StorageStats
	Executed                       bool
	SourcePack                     string
	SourceGone                     bool
	RewrittenBytes, RewrittenCount uint64
	DeferredReason                 string
	NextRun                        time.Time
}

// admitNewWrite is used only for new capture/composition. Accepted retries and
// already durable recovery work remain available to consume reserved headroom.
// Native-only test services without storage maintenance retain their original
// publication contract; OpenLocal always supplies the capacity provider.
func (s *Service) admitNewWrite(ctx context.Context, additional uint64) error {
	if s.maintenance == nil {
		return nil
	}
	policy, err := s.metadata.capacityPolicy(ctx)
	if err != nil {
		return err
	}
	return admitCapacity(ctx, s.maintenance.Capacity, policy, additional)
}

// Collect runs one bounded compaction pass and native collection cycle. It never
// expires a version: expiry is a separate, revalidated policy decision. The
// native engine retains authority over deleted-index and pack safety rules.
func (s *Service) Collect(ctx context.Context, request CollectRequest) (CollectionResult, error) {
	if request.MaxRewriteBytes == 0 {
		request.MaxRewriteBytes = DefaultRewriteBytes
	}
	if request.MaxRewriteBytes > 1<<30 {
		return CollectionResult{}, fmt.Errorf("%w: rewrite budget must not exceed 1 GiB", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return CollectionResult{}, ErrServiceClosed
	}
	if s.maintenance == nil {
		return CollectionResult{}, errors.New("native maintenance is not configured")
	}
	if err := ctx.Err(); err != nil {
		return CollectionResult{}, err
	}
	// Stream close/cancellation releases contentMu without acquiring mu.
	// No new native reader or publisher enters until this cycle is complete.
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	if err := ctx.Err(); err != nil {
		return CollectionResult{}, err
	}
	if err := s.recoverLocked(ctx); err != nil {
		return CollectionResult{}, fmt.Errorf("reconcile project before collection: %w", err)
	}
	pending, exists, err := s.metadata.compaction(ctx)
	if err != nil {
		return CollectionResult{}, err
	}
	policy, err := s.metadata.capacityPolicy(ctx)
	if err != nil {
		return CollectionResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return CollectionResult{}, err
	}
	options := kopia.CollectionOptions{MaxRewriteBytes: request.MaxRewriteBytes}
	// Collection may consume the operational reserve to reclaim unreferenced
	// storage. Only copying live contents requires additional admission. This
	// keeps native GC available when free space has fallen below that reserve.
	capacity, capacityErr := s.maintenance.Capacity(ctx)
	if capacityErr != nil || capacity.FreeB <= policy.ReserveBytes {
		options.MaxRewriteBytes = 0
	} else {
		options.MaxRewriteBytes = min(options.MaxRewriteBytes, (capacity.FreeB-policy.ReserveBytes)/2)
	}
	limitedByCapacity := options.MaxRewriteBytes < request.MaxRewriteBytes
	if exists {
		options.SourcePack = pending.SourcePack
		options.SourceRewritten = !pending.RewrittenAt.IsZero()
	}
	options.BeforeRewrite = func(candidate kopia.PackCandidate) error {
		if candidate.IndexedLiveBytes < 0 || uint64(candidate.IndexedLiveBytes) > math.MaxUint64/2 {
			return ErrCapacityEstimateOverflow
		}
		if err := admitCapacity(ctx, s.maintenance.Capacity, policy, uint64(candidate.IndexedLiveBytes)*2); err != nil {
			return err
		}
		return s.metadata.beginCompaction(ctx, candidate.ID, s.now().UTC())
	}
	options.AfterRewrite = func(source string) error {
		return s.metadata.finishCompactionRewrite(ctx, source, s.now().UTC())
	}
	report, err := s.maintenance.RunCollection(ctx, options)
	result := CollectionResult{
		Before: storageStats(report.Before), After: storageStats(report.After), Executed: report.Executed,
		SourcePack: report.SourcePack, SourceGone: report.SourceGone,
		RewrittenBytes: report.RewrittenBytes, RewrittenCount: report.RewrittenCount,
		DeferredReason: report.DeferredReason, NextRun: report.NextRun,
	}
	if err != nil {
		return result, err
	}
	if limitedByCapacity {
		if result.DeferredReason != "" {
			result.DeferredReason += "; "
		}
		if capacityErr != nil {
			result.DeferredReason += "compaction limited because volume capacity is unavailable"
		} else {
			result.DeferredReason += "compaction limited by volume headroom"
		}
	}
	// A flushed rewrite with no live source references no longer needs a Lode
	// retry checkpoint. Native indexes, roots, sessions, and the persisted
	// rewrite schedule govern eventual orphan deletion. Keeping this singleton
	// until physical deletion would prevent compacting the next pack for days.
	if report.Executed && (report.RewriteComplete || report.SourceGone) && report.SourcePack != "" {
		if err := s.metadata.clearCompaction(ctx, report.SourcePack); err != nil {
			return result, err
		}
	}
	return result, nil
}

func storageStats(stats kopia.RepositoryStats) StorageStats {
	return StorageStats{BlobBytes: stats.BlobBytes, PackBytes: stats.PackBytes, IndexedLiveBytes: stats.IndexedLiveBytes,
		UnreferencedPackBytes: stats.UnreferencedPackBytes, BlobCount: stats.BlobCount, PackCount: stats.PackCount,
		CapacityBytes: stats.CapacityBytes, FreeBytes: stats.FreeBytes, CapacityKnown: stats.CapacityKnown}
}
