package kopia

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotgc"
)

// RepositoryStats reports native storage and content-index measurements. BlobBytes
// and PackBytes are physical blob lengths; IndexedLiveBytes is the packed length
// of non-deleted content records, not unique on-disk bytes (packs can also contain
// deleted contents). UnreferencedPackBytes counts physical p/q pack blobs absent
// from all content-index records, including deleted records. Kopia may retain
// such blobs until its native age and active-session safety checks allow deletion.
// Capacity describes the underlying storage volume, which may be shared with
// files outside this repository.
type RepositoryStats struct {
	BlobBytes             int64
	PackBytes             int64
	IndexedLiveBytes      int64
	UnreferencedPackBytes int64
	BlobCount             int64
	PackCount             int64
	CapacityBytes         uint64
	FreeBytes             uint64
	CapacityKnown         bool
}

// MaintenanceReport captures stats around one invocation of Kopia full maintenance.
// Executed is false when Kopia declined to run because another native maintenance
// lock was held. Safety uses Kopia's fixed SafetyFull policy; this adapter does not
// shorten native age or active-session protections.
type MaintenanceReport struct {
	Before   RepositoryStats
	After    RepositoryStats
	Executed bool
}

// PackCandidate describes one native pack and its measured rewrite economics.
// ObsoleteBytes comes from the pack's embedded original index, compared with
// the current native content index. OriginalOverheadBytes is physical pack size
// minus the original packed-content extents. ReclaimableEstimate subtracts that
// original format overhead from obsolete content bytes; native deletion can
// still wait for the upstream SafetyFull age and session gates.
type PackCandidate struct {
	ID                    string
	PhysicalBytes         int64
	IndexedBytes          int64
	IndexedLiveBytes      int64
	ObsoleteBytes         int64
	OriginalOverheadBytes int64
	ReclaimableEstimate   int64
	LiveContentCount      int
	NextEligibleAt        time.Time
}

// CollectionOptions limits a collection call to at most one native source pack.
// SourcePack resumes a service-persisted candidate; SourceRewritten is a hint
// that the service previously acknowledged no live IDs. Each call rechecks the
// native index because GC may legitimately undelete content afterward.
// BeforeRewrite must persist a newly selected SourcePack before native writes.
// AfterRewrite is called only after native flush succeeds and a fresh index view
// contains no live IDs in that source pack.
type CollectionOptions struct {
	SourcePack      string
	SourceRewritten bool
	MaxRewriteBytes uint64
	BeforeRewrite   func(PackCandidate) error
	AfterRewrite    func(sourcePack string) error
}

// CollectionReport describes one native full-safety collection invocation.
// SourceGone is based on post-run physical blob metadata, not index inference.
type CollectionReport struct {
	Before         RepositoryStats
	After          RepositoryStats
	Executed       bool
	SourcePack     string
	RewrittenBytes uint64
	RewrittenCount uint64
	// RewriteComplete means a refreshed native content index contains no live
	// content in SourcePack. Native maintenance may still retain its physical
	// blob until its own index, age, and active-session safety gates allow deletion.
	RewriteComplete bool
	SourceGone      bool
	DeferredReason  string
	// NextRun is a suggested retry time, not an exact deadline. Native schedule
	// and SafetyFull age/session gates can still defer useful cleanup.
	NextRun                 time.Time
	RewriteScheduleRecorded bool
}

var ErrCollectionSourceChanged = errors.New("native collection source pack changed unexpectedly")

// Maintenance exposes bounded status and native full-maintenance operations.
// Callers must serialize maintenance with writes and gate reads while collection
// is running. Stats require loading the repository's content index and listing its
// blobs; this is currently a full metadata in-memory inspection, not a bounded
// streaming status service.
type Maintenance struct {
	repository repo.DirectRepository
	namespace  string
}

func NewMaintenance(repository repo.DirectRepository) (*Maintenance, error) {
	if repository == nil {
		return nil, errors.New("nil direct repository")
	}
	uniqueID := repository.UniqueID()
	if len(uniqueID) == 0 {
		return nil, errors.New("Kopia repository has an empty unique ID")
	}
	return &Maintenance{repository: repository, namespace: hex.EncodeToString(uniqueID)}, nil
}

func (m *Maintenance) Namespace() string {
	if m == nil {
		return ""
	}
	return m.namespace
}

// Capacity reports the underlying native volume's capacity and available bytes.
// It performs no content-index refresh or blob listing, so callers can use it
// for admission checks without paying the cost of a full repository inspection.
func (m *Maintenance) Capacity(ctx context.Context) (blob.Capacity, error) {
	if m == nil || m.repository == nil {
		return blob.Capacity{}, errors.New("nil maintenance repository")
	}
	if err := ctx.Err(); err != nil {
		return blob.Capacity{}, err
	}
	volume := m.repository.BlobVolume()
	if volume == nil {
		return blob.Capacity{}, blob.ErrNotAVolume
	}
	capacity, err := volume.GetCapacity(ctx)
	if err != nil {
		return blob.Capacity{}, fmt.Errorf("get storage volume capacity: %w", err)
	}
	return capacity, nil
}

// Inspect derives storage measurements from Kopia blob metadata and content-index
// records. It never reads pack or payload bytes.
func (m *Maintenance) Inspect(ctx context.Context) (RepositoryStats, error) {
	if m == nil || m.repository == nil {
		return RepositoryStats{}, errors.New("nil maintenance repository")
	}
	if err := ctx.Err(); err != nil {
		return RepositoryStats{}, err
	}
	if err := m.repository.Refresh(ctx); err != nil {
		return RepositoryStats{}, fmt.Errorf("refresh repository indexes: %w", err)
	}

	var result RepositoryStats
	usedPacks := make(map[blob.ID]struct{})
	if err := m.repository.ContentReader().IterateContents(ctx, content.IterateOptions{IncludeDeleted: true}, func(info content.Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		usedPacks[info.PackBlobID] = struct{}{}
		if !info.Deleted {
			if err := addInt64(&result.IndexedLiveBytes, int64(info.PackedLength)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return RepositoryStats{}, fmt.Errorf("iterate content index: %w", err)
	}

	reader := m.repository.BlobReader()
	if err := reader.ListBlobs(ctx, "", func(meta blob.Metadata) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if meta.Length < 0 {
			return fmt.Errorf("blob %q has negative length %d", meta.BlobID, meta.Length)
		}
		if err := addInt64(&result.BlobBytes, meta.Length); err != nil {
			return err
		}
		if result.BlobCount == math.MaxInt64 {
			return errors.New("blob count overflow")
		}
		result.BlobCount++
		return nil
	}); err != nil {
		return RepositoryStats{}, fmt.Errorf("list repository blobs: %w", err)
	}

	for _, prefix := range content.PackBlobIDPrefixes {
		if err := reader.ListBlobs(ctx, prefix, func(meta blob.Metadata) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if meta.Length < 0 {
				return fmt.Errorf("pack blob %q has negative length %d", meta.BlobID, meta.Length)
			}
			if err := addInt64(&result.PackBytes, meta.Length); err != nil {
				return err
			}
			if result.PackCount == math.MaxInt64 {
				return errors.New("pack count overflow")
			}
			result.PackCount++
			if _, found := usedPacks[meta.BlobID]; !found {
				if err := addInt64(&result.UnreferencedPackBytes, meta.Length); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return RepositoryStats{}, fmt.Errorf("list pack blobs with prefix %q: %w", prefix, err)
		}
	}

	capacity, err := m.Capacity(ctx)
	if err == nil {
		result.CapacityBytes = capacity.SizeB
		result.FreeBytes = capacity.FreeB
		result.CapacityKnown = true
	} else if !errors.Is(err, blob.ErrNotAVolume) {
		return RepositoryStats{}, err
	}
	return result, nil
}

// RunFull runs Kopia's snapshot GC and repository full maintenance with the
// upstream SafetyFull age/session rules. It reports a lock-skipped invocation
// as Executed=false rather than implying collection occurred.
func (m *Maintenance) RunFull(ctx context.Context) (MaintenanceReport, error) {
	if m == nil || m.repository == nil {
		return MaintenanceReport{}, errors.New("nil maintenance repository")
	}
	before, err := m.Inspect(ctx)
	if err != nil {
		return MaintenanceReport{}, err
	}
	var report MaintenanceReport
	report.Before = before
	if err := repo.DirectWriteSession(ctx, m.repository, repo.WriteSessionOptions{Purpose: "Lode full maintenance"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
		if writer.ClientOptions().ReadOnly {
			return errors.New("cannot run maintenance on a read-only repository")
		}
		return maintenance.RunExclusive(ctx, writer, maintenance.ModeFull, false, func(ctx context.Context, params maintenance.RunParameters) error {
			report.Executed = true
			if err := snapshotgc.Run(ctx, writer, true, maintenance.SafetyFull, params.MaintenanceStartTime); err != nil {
				return fmt.Errorf("native snapshot garbage collection: %w", err)
			}
			if err := maintenance.Run(ctx, params, maintenance.SafetyFull); err != nil {
				return fmt.Errorf("native full maintenance: %w", err)
			}
			return nil
		})
	}); err != nil {
		return MaintenanceReport{}, fmt.Errorf("run native full maintenance: %w", err)
	}
	after, err := m.Inspect(ctx)
	if err != nil {
		return MaintenanceReport{}, err
	}
	report.After = after
	return report, nil
}

func addInt64(dst *int64, value int64) error {
	if value < 0 || *dst > math.MaxInt64-value {
		return errors.New("repository byte count overflow")
	}
	*dst += value
	return nil
}
