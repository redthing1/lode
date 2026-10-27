package kopia

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/maintenancestats"
	"github.com/kopia/kopia/snapshot/snapshotgc"
)

// RunCollection performs one full-safety native collection run with at most one
// source pack's live contents explicitly rewritten. The service should hold its
// mutation lock and reader gate for the whole call. BeforeRewrite is invoked for
// every actual copy, including resumed copies, before native rewrite IO begins.
// AfterRewrite is invoked after the rewrite and its native schedule record are
// flushed, once a current native index view has no live IDs in the source pack.
//
// A truthful native full-rewrite schedule entry for this bounded operation
// prevents maintenance.Run from selecting its unbounded automatic rewrite in
// the same full run. Native snapshot GC, deleted-index cleanup, and age/session
// checked orphan deletion remain upstream operations with unchanged SafetyFull.
func (m *Maintenance) RunCollection(ctx context.Context, options CollectionOptions) (CollectionReport, error) {
	if m == nil || m.repository == nil {
		return CollectionReport{}, errors.New("nil maintenance repository")
	}
	if options.SourcePack != "" && !isPackBlobID(options.SourcePack) {
		return CollectionReport{}, fmt.Errorf("invalid collection source pack %q", options.SourcePack)
	}
	if options.SourcePack == "" && options.SourceRewritten {
		return CollectionReport{}, errors.New("rewritten collection source pack is required")
	}
	before, err := m.Inspect(ctx)
	if err != nil {
		return CollectionReport{}, err
	}
	report := CollectionReport{Before: before, SourcePack: options.SourcePack}
	var deferredReason string
	var nextRun time.Time
	var sourceCandidate *nativePackSnapshot
	var sourcePack string
	var acknowledgeRewrite bool
	var didRewrite bool

	err = repo.DirectWriteSession(ctx, m.repository, repo.WriteSessionOptions{Purpose: "Lode bounded native collection"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
		if writer.ClientOptions().ReadOnly {
			return errors.New("cannot collect a read-only repository")
		}
		if err := initializeNativeMaintenanceOwner(ctx, writer); err != nil {
			return err
		}
		return maintenance.RunExclusive(ctx, writer, maintenance.ModeFull, false, func(ctx context.Context, params maintenance.RunParameters) error {
			report.Executed = true
			if err := snapshotgc.Run(ctx, writer, true, maintenance.SafetyFull, params.MaintenanceStartTime); err != nil {
				return fmt.Errorf("native snapshot garbage collection: %w", err)
			}

			if options.SourcePack != "" {
				// A persisted source is a recovery checkpoint. Its economics may
				// have changed since selection (for example, undeleted contents can
				// become live again), so resume it from the remaining native IDs and
				// current age/budget constraints without applying the new-candidate
				// cost gate again.
				sourceCandidate, err = inspectNativePack(ctx, writer, options.SourcePack)
				if err != nil {
					return err
				}
				sourcePack = options.SourcePack
			} else if options.MaxRewriteBytes > 0 {
				sourceCandidate, deferredReason, nextRun, err = selectNativePack(ctx, writer, writer.ContentManager(), options.MaxRewriteBytes, writer.Time())
				if err != nil {
					return err
				}
				if sourceCandidate != nil {
					sourcePack = sourceCandidate.candidate.ID
					report.SourcePack = sourcePack
				}
			}

			var liveInfos []content.Info
			if sourceCandidate != nil {
				liveInfos = append(liveInfos, sourceCandidate.liveInfos...)
			}
			liveBytes := int64(0)
			var rewriteIDs []content.ID
			for _, info := range liveInfos {
				if err := addInt64(&liveBytes, int64(info.PackedLength)); err != nil {
					return err
				}
				if writer.Time().Sub(info.Timestamp()) >= maintenance.SafetyFull.RewriteMinAge {
					rewriteIDs = append(rewriteIDs, info.ContentID)
				}
			}

			costQualified := options.SourcePack != "" || sourceCandidate != nil && meetsWasteThreshold(sourceCandidate.candidate)
			if sourcePack != "" && sourceCandidate != nil && costQualified && len(rewriteIDs) > 0 &&
				options.MaxRewriteBytes > 0 && uint64(liveBytes) <= options.MaxRewriteBytes {
				if options.BeforeRewrite == nil {
					return errors.New("BeforeRewrite callback is required before native rewrite")
				}
				if options.AfterRewrite == nil {
					return errors.New("AfterRewrite callback is required before native rewrite")
				}
				if sourceCandidate == nil {
					return fmt.Errorf("%w: source pack %q is not present in the native index", ErrCollectionSourceChanged, sourcePack)
				}
				// This runs for first selection and every retry. It lets the service
				// recheck headroom and idempotently persist/reopen its rewrite phase.
				if err := options.BeforeRewrite(sourceCandidate.candidate); err != nil {
					return fmt.Errorf("persist or resume collection source pack: %w", err)
				}
				for _, info := range liveInfos {
					if string(info.PackBlobID) != sourcePack {
						return fmt.Errorf("%w: content %q moved out of source pack", ErrCollectionSourceChanged, info.ContentID)
					}
				}
				var rewriteResult *maintenancestats.RewriteContentsStats
				if err := reportNativeRewrite(ctx, writer, func() (maintenancestats.Kind, error) {
					stats, err := maintenance.RewriteContents(ctx, writer, &maintenance.RewriteContentsOptions{
						ContentIDs: rewriteIDs,
						Parallel:   1,
					}, maintenance.SafetyFull)
					rewriteResult = stats
					return stats, err
				}); err != nil {
					return fmt.Errorf("rewrite source pack %q: %w", sourcePack, err)
				}
				didRewrite = true
				report.RewriteScheduleRecorded = true
				if rewriteResult != nil {
					report.RewrittenBytes = rewriteResult.RewrittenContentSize
					report.RewrittenCount = rewriteResult.RewrittenContentCount
				}
			} else if sourcePack != "" && len(liveInfos) > 0 {
				switch {
				case options.SourcePack == "" && sourceCandidate != nil && !meetsWasteThreshold(sourceCandidate.candidate):
					deferredReason = "source pack does not pass the native copy-cost gate"
				case options.MaxRewriteBytes == 0 || uint64(liveBytes) > options.MaxRewriteBytes:
					deferredReason = "live source bytes exceed rewrite budget"
				case len(rewriteIDs) == 0:
					deferredReason = "source contents have not reached native rewrite age"
					nextRun = sourceCandidate.candidate.NextEligibleAt
				default:
					deferredReason = "source pack still contains live contents"
				}
			}

			// RunExclusive's lock is the serialization boundary for checking the
			// native schedule. ReportRun ignores SetSchedule errors, so explicitly
			// persist and reload each bounded marker before allowing maintenance.Run.
			schedule, err := maintenance.GetSchedule(ctx, writer)
			if err != nil {
				return fmt.Errorf("load maintenance schedule before native full run: %w", err)
			}
			if fullRewriteWouldRun(schedule, maintenance.SafetyFull) && !report.RewriteScheduleRecorded {
				if err := reportNativeRewrite(ctx, writer, func() (maintenancestats.Kind, error) {
					// The pack inspection above is a real bounded scan. A zero-work
					// result records that this pass intentionally selected no contents;
					// it must never call RewriteContents with empty ContentIDs.
					return &maintenancestats.RewriteContentsStats{}, nil
				}); err != nil {
					return fmt.Errorf("record bounded no-work rewrite scan: %w", err)
				}
				report.RewriteScheduleRecorded = true
				schedule, err = maintenance.GetSchedule(ctx, writer)
				if err != nil {
					return fmt.Errorf("reload maintenance schedule after no-work scan: %w", err)
				}
			}
			if report.RewriteScheduleRecorded {
				if err := verifyFullRewriteMarker(schedule); err != nil {
					return err
				}
			}
			if fullRewriteWouldRun(schedule, maintenance.SafetyFull) {
				return errors.New("native full maintenance would run an unbounded content rewrite")
			}

			if sourcePack != "" && (!options.SourceRewritten || didRewrite) {
				remaining, err := liveContentInfosInPack(ctx, writer, sourcePack)
				if err != nil {
					return err
				}
				if len(remaining) == 0 {
					if options.AfterRewrite == nil {
						return errors.New("AfterRewrite callback is required to acknowledge an empty source pack")
					}
					// The callback commits RewrittenAt before the full run can drop
					// indexes or delete the source pack. A crash afterward is resumed
					// from that durable phase and checks physical SourceGone separately.
					if err := options.AfterRewrite(sourcePack); err != nil {
						return fmt.Errorf("persist completed source rewrite: %w", err)
					}
					acknowledgeRewrite = true
				} else {
					deferredReason = "source pack still contains live contents"
					nextRun = nextAllLiveEligibleAt(remaining)
				}
			}
			return maintenance.Run(ctx, params, maintenance.SafetyFull)
		})
	})
	if err != nil {
		return CollectionReport{}, fmt.Errorf("run bounded native collection: %w", err)
	}
	if !report.Executed {
		// The native lock was busy. No rewrite checkpoint was reconciled and
		// the post-run completion checks must not infer that one was.
		report.DeferredReason = "native maintenance lock is busy"
		report.After, err = m.Inspect(ctx)
		return report, err
	}
	if err := m.repository.Refresh(ctx); err != nil {
		return CollectionReport{}, fmt.Errorf("refresh repository after bounded collection: %w", err)
	}
	if report.SourcePack != "" {
		meta, err := m.repository.BlobReader().GetMetadata(ctx, blob.ID(report.SourcePack))
		if errors.Is(err, blob.ErrBlobNotFound) {
			report.SourceGone = true
		} else if err != nil {
			return CollectionReport{}, fmt.Errorf("inspect source pack after collection: %w", err)
		}
		remaining, err := liveContentInfosInPack(ctx, m.repository, report.SourcePack)
		if err != nil {
			return CollectionReport{}, err
		}
		report.RewriteComplete = len(remaining) == 0
		if !report.SourceGone {
			if report.RewriteComplete {
				deferredReason = "waiting for native index, pack age, or session safety"
			}
			nativeNext := nextNativeCollectionTime(ctx, m.repository, meta, report.SourcePack != "")
			if nativeNext.After(nextRun) {
				nextRun = nativeNext
			}
		}
	}
	if !acknowledgeRewrite && report.SourcePack != "" && !options.SourceRewritten {
		remaining, err := liveContentInfosInPack(ctx, m.repository, report.SourcePack)
		if err != nil {
			return CollectionReport{}, err
		}
		if len(remaining) == 0 {
			return CollectionReport{}, errors.New("source pack reached zero live contents without a durable rewrite acknowledgement")
		}
	}
	if schedule, err := maintenance.GetSchedule(ctx, m.repository); err == nil {
		cooldown := latestSuccessfulRun(schedule.Runs[maintenance.TaskRewriteContentsFull]).Add(maintenance.SafetyFull.MinRewriteToOrphanDeletionDelay)
		if cooldown.After(nextRun) {
			nextRun = cooldown
		}
		if schedule.NextFullMaintenanceTime.After(nextRun) {
			nextRun = schedule.NextFullMaintenanceTime
		}
	}
	report.NextRun = nextRun
	report.DeferredReason = deferredReason
	after, err := m.Inspect(ctx)
	if err != nil {
		return CollectionReport{}, err
	}
	report.After = after
	return report, nil
}

type nativePackSnapshot struct {
	candidate PackCandidate
	liveInfos []content.Info
}

func inspectNativePack(ctx context.Context, rep repo.DirectRepository, wanted string) (*nativePackSnapshot, error) {
	var found *nativePackSnapshot
	err := rep.ContentReader().IteratePacks(ctx, content.IteratePackOptions{
		Prefixes:                           content.PackBlobIDPrefixes,
		IncludePacksWithOnlyDeletedContent: true,
		IncludeContentInfos:                true,
	}, func(info content.PackInfo) error {
		if string(info.PackID) != wanted {
			return nil
		}
		var err error
		found, err = makeNativePackSnapshot(ctx, rep, info)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("inspect native pack %q: %w", wanted, err)
	}
	return found, nil
}

func selectNativePack(ctx context.Context, rep repo.DirectRepository, contentManager *content.WriteManager, maxBytes uint64, now time.Time) (*nativePackSnapshot, string, time.Time, error) {
	var best *nativePackSnapshot
	var budgetDeferred *nativePackSnapshot
	var ageDeferred *nativePackSnapshot
	var costDeferred bool
	err := rep.ContentReader().IteratePacks(ctx, content.IteratePackOptions{
		Prefixes:                           content.PackBlobIDPrefixes,
		IncludePacksWithOnlyDeletedContent: true,
		IncludeContentInfos:                true,
	}, func(info content.PackInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, err := makeNativePackSnapshot(ctx, rep, info)
		if err != nil {
			return err
		}
		if !possibleNativeWaste(candidate.candidate) {
			return nil
		}
		if maxBytes == 0 || uint64(candidate.candidate.IndexedLiveBytes) > maxBytes {
			if betterPack(candidate, budgetDeferred) {
				budgetDeferred = candidate
			}
			return nil
		}
		allOld := true
		for _, live := range candidate.liveInfos {
			if now.Sub(live.Timestamp()) < maintenance.SafetyFull.RewriteMinAge {
				allOld = false
				break
			}
		}
		if !allOld {
			if ageDeferred == nil || candidate.candidate.NextEligibleAt.Before(ageDeferred.candidate.NextEligibleAt) {
				ageDeferred = candidate
			}
			return nil
		}
		if err := inspectOriginalPackCost(ctx, rep, contentManager, candidate); err != nil {
			return err
		}
		if !meetsWasteThreshold(candidate.candidate) {
			costDeferred = true
			return nil
		}
		if betterPack(candidate, best) {
			best = candidate
		}
		return nil
	})
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("scan native pack candidates: %w", err)
	}
	if best != nil {
		return best, "", time.Time{}, nil
	}
	if ageDeferred != nil {
		return nil, "candidate contents have not reached native rewrite age", ageDeferred.candidate.NextEligibleAt, nil
	}
	if budgetDeferred != nil {
		return nil, "candidate live bytes exceed rewrite budget", time.Time{}, nil
	}
	if costDeferred {
		return nil, "no pack passes the native copy-cost gate", time.Time{}, nil
	}
	return nil, "no pack has sufficient obsolete content", time.Time{}, nil
}

func makeNativePackSnapshot(ctx context.Context, rep repo.DirectRepository, info content.PackInfo) (*nativePackSnapshot, error) {
	meta, err := rep.BlobReader().GetMetadata(ctx, info.PackID)
	if err != nil {
		return nil, fmt.Errorf("get physical metadata for pack %q: %w", info.PackID, err)
	}
	if meta.Length < 0 {
		return nil, fmt.Errorf("pack %q has negative physical length %d", info.PackID, meta.Length)
	}
	candidate := PackCandidate{ID: string(info.PackID), PhysicalBytes: meta.Length}
	snapshot := &nativePackSnapshot{candidate: candidate}
	for _, contentInfo := range info.ContentInfos {
		if err := addInt64(&snapshot.candidate.IndexedBytes, int64(contentInfo.PackedLength)); err != nil {
			return nil, err
		}
		if contentInfo.Deleted {
			continue
		}
		if err := addInt64(&snapshot.candidate.IndexedLiveBytes, int64(contentInfo.PackedLength)); err != nil {
			return nil, err
		}
		snapshot.liveInfos = append(snapshot.liveInfos, contentInfo)
	}
	snapshot.candidate.LiveContentCount = len(snapshot.liveInfos)
	snapshot.candidate.ReclaimableEstimate = max(snapshot.candidate.PhysicalBytes-snapshot.candidate.IndexedLiveBytes, 0)
	snapshot.candidate.NextEligibleAt = nextAllLiveEligibleAt(snapshot.liveInfos)
	return snapshot, nil
}

func possibleNativeWaste(candidate PackCandidate) bool {
	if candidate.LiveContentCount == 0 || candidate.PhysicalBytes <= candidate.IndexedLiveBytes {
		return false
	}
	possibleObsolete := candidate.PhysicalBytes - candidate.IndexedLiveBytes
	return possibleObsolete >= ceilThird(candidate.IndexedLiveBytes)
}

func meetsWasteThreshold(candidate PackCandidate) bool {
	if candidate.LiveContentCount == 0 || candidate.ObsoleteBytes <= candidate.OriginalOverheadBytes {
		return false
	}
	return candidate.ObsoleteBytes >= ceilThird(candidate.IndexedLiveBytes)
}

func ceilThird(value int64) int64 {
	result := value / 3
	if value%3 != 0 {
		result++
	}
	return result
}

func inspectOriginalPackCost(ctx context.Context, rep repo.DirectRepository, contentManager *content.WriteManager, snapshot *nativePackSnapshot) error {
	if contentManager == nil {
		return errors.New("native content manager is required to inspect pack recovery index")
	}
	physical := snapshot.candidate.PhysicalBytes
	original, err := contentManager.RecoverIndexFromPackBlob(ctx, blob.ID(snapshot.candidate.ID), physical, false)
	if err != nil {
		return fmt.Errorf("recover native original index for pack %q: %w", snapshot.candidate.ID, err)
	}
	if len(original) == 0 {
		return fmt.Errorf("native original index for pack %q is empty", snapshot.candidate.ID)
	}
	ordered := append([]content.Info(nil), original...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].PackOffset == ordered[j].PackOffset {
			return ordered[i].ContentID.String() < ordered[j].ContentID.String()
		}
		return ordered[i].PackOffset < ordered[j].PackOffset
	})
	originalByID := make(map[content.ID]content.Info, len(ordered))
	var originalBytes, obsoleteBytes, currentLiveBytes int64
	var previousEnd uint64
	for i, info := range ordered {
		if info.ContentID.String() == "" || info.PackBlobID != blob.ID(snapshot.candidate.ID) || info.PackedLength == 0 {
			return fmt.Errorf("invalid original content extent in pack %q: %+v", snapshot.candidate.ID, info)
		}
		if _, found := originalByID[info.ContentID]; found {
			return fmt.Errorf("duplicate content ID %q in original index for pack %q", info.ContentID, snapshot.candidate.ID)
		}
		start := uint64(info.PackOffset)
		end := start + uint64(info.PackedLength)
		if end < start || end > uint64(physical) {
			return fmt.Errorf("original content extent exceeds pack %q: offset=%d length=%d physical=%d", snapshot.candidate.ID, info.PackOffset, info.PackedLength, physical)
		}
		if i > 0 && start < previousEnd {
			return fmt.Errorf("overlapping original content extents in pack %q at offset %d", snapshot.candidate.ID, start)
		}
		previousEnd = end
		if err := addInt64(&originalBytes, int64(info.PackedLength)); err != nil {
			return err
		}
		current, lookupErr := rep.ContentReader().ContentInfo(ctx, info.ContentID)
		if lookupErr != nil && !errors.Is(lookupErr, content.ErrContentNotFound) {
			return fmt.Errorf("lookup current native content %q: %w", info.ContentID, lookupErr)
		}
		stillLiveHere := lookupErr == nil && !current.Deleted && current.PackBlobID == blob.ID(snapshot.candidate.ID) && current.PackOffset == info.PackOffset && current.PackedLength == info.PackedLength
		if stillLiveHere {
			if err := addInt64(&currentLiveBytes, int64(info.PackedLength)); err != nil {
				return err
			}
		} else if err := addInt64(&obsoleteBytes, int64(info.PackedLength)); err != nil {
			return err
		}
		originalByID[info.ContentID] = info
	}
	if originalBytes > physical {
		return fmt.Errorf("original indexed content bytes exceed physical pack %q: indexed=%d physical=%d", snapshot.candidate.ID, originalBytes, physical)
	}
	if currentLiveBytes != snapshot.candidate.IndexedLiveBytes {
		return fmt.Errorf("native current live-byte mismatch for pack %q: original-location=%d current-pack-index=%d", snapshot.candidate.ID, currentLiveBytes, snapshot.candidate.IndexedLiveBytes)
	}
	for _, live := range snapshot.liveInfos {
		originalInfo, found := originalByID[live.ContentID]
		if !found || originalInfo.PackOffset != live.PackOffset || originalInfo.PackedLength != live.PackedLength {
			return fmt.Errorf("current live content %q is absent or mismatched in original pack index %q", live.ContentID, snapshot.candidate.ID)
		}
	}
	snapshot.candidate.ObsoleteBytes = obsoleteBytes
	snapshot.candidate.OriginalOverheadBytes = physical - originalBytes
	snapshot.candidate.ReclaimableEstimate = max(obsoleteBytes-snapshot.candidate.OriginalOverheadBytes, 0)
	return nil
}

func betterPack(candidate, current *nativePackSnapshot) bool {
	if current == nil || candidate.candidate.ReclaimableEstimate != current.candidate.ReclaimableEstimate {
		return current == nil || candidate.candidate.ReclaimableEstimate > current.candidate.ReclaimableEstimate
	}
	return candidate.candidate.ID < current.candidate.ID
}

func reportNativeRewrite(ctx context.Context, writer repo.DirectRepositoryWriter, run func() (maintenancestats.Kind, error)) error {
	schedule, err := maintenance.GetSchedule(ctx, writer)
	if err != nil {
		return fmt.Errorf("load maintenance schedule before bounded rewrite: %w", err)
	}
	if err := maintenance.ReportRun(ctx, writer, maintenance.TaskRewriteContentsFull, schedule, run); err != nil {
		if saveErr := maintenance.SetSchedule(ctx, writer, schedule); saveErr != nil {
			return errors.Join(err, fmt.Errorf("persist failed rewrite run marker: %w", saveErr))
		}
		return err
	}
	// ReportRun currently logs and swallows schedule persistence failures.
	if err := maintenance.SetSchedule(ctx, writer, schedule); err != nil {
		return fmt.Errorf("persist bounded rewrite schedule: %w", err)
	}
	return nil
}

func verifyFullRewriteMarker(schedule *maintenance.Schedule) error {
	latestRewrite := latestSuccessfulRun(schedule.Runs[maintenance.TaskRewriteContentsFull])
	latestDelete := latestSuccessfulRun(schedule.Runs[maintenance.TaskDeleteOrphanedBlobsFull], schedule.Runs[maintenance.TaskDeleteOrphanedBlobsQuick])
	if !latestRewrite.After(latestDelete) {
		return fmt.Errorf("bounded rewrite marker end %s is not strictly newer than native delete marker %s; refusing full maintenance", latestRewrite, latestDelete)
	}
	return nil
}

func fullRewriteWouldRun(schedule *maintenance.Schedule, safety maintenance.SafetyParameters) bool {
	latestRewrite := latestSuccessfulRun(schedule.Runs[maintenance.TaskRewriteContentsFull])
	latestDelete := latestSuccessfulRun(schedule.Runs[maintenance.TaskDeleteOrphanedBlobsFull], schedule.Runs[maintenance.TaskDeleteOrphanedBlobsQuick])
	if latestRewrite.IsZero() || safety.MinRewriteToOrphanDeletionDelay == 0 {
		return true
	}
	return !latestDelete.Before(latestRewrite)
}

func latestSuccessfulRun(runs ...[]maintenance.RunInfo) time.Time {
	var latest time.Time
	for _, group := range runs {
		for _, run := range group {
			if run.Success && run.End.After(latest) {
				latest = run.End
			}
		}
	}
	return latest
}

func liveContentInfosInPack(ctx context.Context, rep repo.DirectRepository, source string) ([]content.Info, error) {
	var result []content.Info
	err := rep.ContentReader().IterateContents(ctx, content.IterateOptions{IncludeDeleted: true}, func(info content.Info) error {
		if info.PackBlobID == blob.ID(source) && !info.Deleted {
			result = append(result, info)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list live source-pack contents: %w", err)
	}
	return result, nil
}

func nextAllLiveEligibleAt(infos []content.Info) time.Time {
	var next time.Time
	for _, info := range infos {
		eligible := info.Timestamp().Add(maintenance.SafetyFull.RewriteMinAge)
		if next.IsZero() || eligible.After(next) {
			next = eligible
		}
	}
	return next
}

func nextNativeCollectionTime(ctx context.Context, rep repo.DirectRepository, source blob.Metadata, hasSource bool) time.Time {
	var next time.Time
	if schedule, err := maintenance.GetSchedule(ctx, rep); err == nil {
		next = schedule.NextFullMaintenanceTime
		latestRewrite := latestSuccessfulRun(schedule.Runs[maintenance.TaskRewriteContentsFull])
		if !latestRewrite.IsZero() {
			cooldown := latestRewrite.Add(maintenance.SafetyFull.MinRewriteToOrphanDeletionDelay)
			if cooldown.After(next) {
				next = cooldown
			}
		}
	}
	if hasSource {
		ageGate := source.Timestamp.Add(maintenance.SafetyFull.PackDeleteMinAge)
		if ageGate.After(next) {
			next = ageGate
		}
	}
	return next
}

func isPackBlobID(id string) bool {
	if len(id) < 2 || len(id) > 256 || (id[0] != 'p' && id[0] != 'q') {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func initializeNativeMaintenanceOwner(ctx context.Context, writer repo.DirectRepositoryWriter) error {
	hasParams, err := maintenance.HasParams(ctx, writer)
	if err != nil {
		return fmt.Errorf("inspect native maintenance ownership: %w", err)
	}
	params, err := maintenance.GetParams(ctx, writer)
	if err != nil {
		return fmt.Errorf("load native maintenance parameters: %w", err)
	}
	if hasParams && params.Owner != "" {
		return nil
	}
	owner := writer.ClientOptions().UsernameAtHost()
	if owner == "" {
		return errors.New("cannot initialize native maintenance owner without a client identity")
	}
	params.Owner = owner
	// Lode is the explicit maintenance coordinator. Do not turn on background
	// automatic cycles while assigning the native ownership marker.
	params.QuickCycle.Enabled = false
	params.FullCycle.Enabled = false
	if err := maintenance.SetParams(ctx, writer, params); err != nil {
		return fmt.Errorf("initialize native maintenance ownership: %w", err)
	}
	return nil
}
