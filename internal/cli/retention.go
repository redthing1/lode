package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/workspace"
)

func runTag(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("tag requires create, delete, or list")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) != 1 {
			return errors.New("tag help takes no arguments")
		}
		_, err := io.WriteString(stdout, "Usage: lode tag create|delete|list [options]\nUse 'lode tag create --help' or 'lode tag delete --help' for options.\n")
		return err
	}
	switch args[0] {
	case "create":
		fs := newFlags("tag create [-C DIR] [--from COMMIT] NAME", stderr)
		directory := fs.String("C", ".", "workspace directory")
		from := fs.String("from", "", "commit ID (default: workspace base)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			return errors.New("tag create requires exactly one NAME")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		if err := w.CreateTag(ctx, fs.Arg(0), *from); err != nil {
			return fmt.Errorf("create tag: %w", err)
		}
		tag, err := w.Tag(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "tagged %s as %s\n", tag.CommitID, tag.Name)
		return err
	case "delete":
		fs := newFlags("tag delete [-C DIR] NAME", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			return errors.New("tag delete requires exactly one NAME")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		if err := w.DeleteTag(ctx, fs.Arg(0)); err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "deleted tag %s\n", fs.Arg(0))
		return err
	case "list":
		fs := newFlags("tag list [-C DIR]", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("tag list takes no positional arguments")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		for after := ""; ; {
			tags, next, err := w.Tags(ctx, after, 256)
			if err != nil {
				return fmt.Errorf("list tags: %w", err)
			}
			for _, tag := range tags {
				if _, err := fmt.Fprintf(stdout, "%s %s\n", tag.Name, tag.CommitID); err != nil {
					return err
				}
			}
			if next == "" {
				return nil
			}
			after = next
		}
	default:
		return fmt.Errorf("unknown tag command %q", args[0])
	}
}

func runRetention(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("retention requires show, set, preview, apply, or why")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) != 1 {
			return errors.New("retention help takes no arguments")
		}
		_, err := io.WriteString(stdout, "Usage: lode retention show|set|preview|apply|why [options]\nUse 'lode retention COMMAND --help' for command options.\n")
		return err
	}
	switch args[0] {
	case "show":
		fs := newFlags("retention show [-C DIR]", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("retention show takes no positional arguments")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		policy, err := w.RetentionPolicy(ctx)
		if err != nil {
			return fmt.Errorf("read retention policy: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "keep-last: %d\nkeep-days: %d\n", policy.KeepLast, policy.KeepDays)
		return err
	case "set":
		fs := newFlags("retention set [-C DIR] --last N --days N", stderr)
		directory := fs.String("C", ".", "workspace directory")
		keepLast := fs.Int("last", -1, "first-parent commits to keep per branch (0 disables)")
		keepDays := fs.Int("days", -1, "recent commit window in days (0 disables)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *keepLast < 0 || *keepDays < 0 {
			return errors.New("retention set requires nonnegative --last and --days values")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		if err := w.SetRetentionPolicy(ctx, project.RetentionPolicy{KeepLast: *keepLast, KeepDays: *keepDays}); err != nil {
			return fmt.Errorf("set retention policy: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "retention policy set: keep-last %d, keep-days %d\n", *keepLast, *keepDays)
		return err
	case "preview":
		fs := newFlags("retention preview [-C DIR]", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("retention preview takes no positional arguments")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		var total int
		for after := ""; ; {
			page, err := w.PreviewRetention(ctx, after, 256)
			if err != nil {
				return fmt.Errorf("preview retention: %w", err)
			}
			if after == "" {
				total = page.TotalCandidates
				if _, err := fmt.Fprintf(stdout, "candidates: %d\nevaluated: %s\n", total, page.EvaluatedAt.UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
			for _, candidate := range page.Candidates {
				if _, err := fmt.Fprintf(stdout, "%s %s\n", candidate.CommitID, candidate.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
			if page.Next == "" {
				return nil
			}
			after = page.Next
		}
	case "apply":
		fs := newFlags("retention apply [-C DIR] [--limit N]", stderr)
		directory := fs.String("C", ".", "workspace directory")
		limit := fs.Int("limit", 256, "maximum candidates to expire (1–1000)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *limit < 1 || *limit > 1000 {
			return errors.New("retention apply --limit must be between 1 and 1000")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		preview, err := w.PreviewRetention(ctx, "", *limit)
		if err != nil {
			return fmt.Errorf("preview retention: %w", err)
		}
		ids := make([]string, 0, len(preview.Candidates))
		for _, candidate := range preview.Candidates {
			ids = append(ids, candidate.CommitID)
		}
		if len(ids) == 0 {
			_, err = io.WriteString(stdout, "no eligible payloads\n")
			return err
		}
		result, err := w.Expire(ctx, ids)
		if err != nil {
			return fmt.Errorf("expire payloads: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "expired payload versions: %d\n", len(result.Expired)); err != nil {
			return err
		}
		for _, id := range result.Expired {
			if _, err := fmt.Fprintf(stdout, "expired %s\n", id); err != nil {
				return err
			}
		}
		for _, version := range result.Protected {
			if _, err := fmt.Fprintf(stdout, "protected %s: %s\n", version.CommitID, strings.Join(version.Reasons, ", ")); err != nil {
				return err
			}
		}
		_, err = io.WriteString(stdout, "Physical collection is separate.\n")
		return err
	case "why":
		fs := newFlags("retention why [-C DIR] COMMIT", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("retention why requires exactly one COMMIT")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		availability, err := w.ExplainRetention(ctx, fs.Arg(0))
		if err != nil {
			return fmt.Errorf("explain retention: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "state: %s\n", availability.State); err != nil {
			return err
		}
		if !availability.ExpiredAt.IsZero() {
			if _, err := fmt.Fprintf(stdout, "expired: %s\nrelease-pending: %t\n", availability.ExpiredAt.UTC().Format(time.RFC3339), availability.ReleasePending); err != nil {
				return err
			}
		}
		for _, reason := range availability.Reasons {
			if _, err := fmt.Fprintf(stdout, "reason: %s\n", reason); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown retention command %q", args[0])
	}
}

func runStorage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("storage requires status, collect, or capacity")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) != 1 {
			return errors.New("storage help takes no arguments")
		}
		_, err := io.WriteString(stdout, "Usage: lode storage status|collect|capacity [options]\n")
		return err
	}
	switch args[0] {
	case "status":
		return runStorageStatus(ctx, args[1:], stdout, stderr)
	case "collect":
		return runStorageCollect(ctx, args[1:], stdout, stderr)
	case "capacity":
		return runStorageCapacity(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown storage command %q", args[0])
	}
}

func runStorageStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("storage status [-C DIR]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("storage status takes no positional arguments")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	stats, err := w.StorageStats(ctx)
	if err != nil {
		return fmt.Errorf("read storage stats: %w", err)
	}
	if _, err := fmt.Fprintf(stdout,
		"blob-bytes: %d\npack-bytes: %d\nindexed-live-bytes: %d\nunreferenced-pack-bytes: %d\nblob-count: %d\npack-count: %d\n",
		stats.BlobBytes, stats.PackBytes, stats.IndexedLiveBytes, stats.UnreferencedPackBytes, stats.BlobCount, stats.PackCount); err != nil {
		return err
	}
	if stats.CapacityKnown {
		_, err = fmt.Fprintf(stdout, "volume-capacity-bytes: %d\nvolume-free-bytes: %d\n", stats.CapacityBytes, stats.FreeBytes)
		return err
	}
	_, err = io.WriteString(stdout, "volume-capacity: unavailable\n")
	return err
}

func runStorageCollect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("storage collect [-C DIR] [--max-rewrite-bytes N]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	maxRewriteBytes := fs.Uint64("max-rewrite-bytes", project.DefaultRewriteBytes, "maximum source bytes to rewrite (1–1 GiB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *maxRewriteBytes == 0 || *maxRewriteBytes > 1<<30 {
		return errors.New("storage collect --max-rewrite-bytes must be between 1 byte and 1 GiB")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	result, err := w.Collect(ctx, project.CollectRequest{MaxRewriteBytes: *maxRewriteBytes})
	if err != nil {
		return fmt.Errorf("collect storage: %w", err)
	}
	if _, err := fmt.Fprintf(stdout,
		"collection-executed: %t\nblob-bytes-before: %d\nblob-bytes-after: %d\nrewritten-bytes: %d\nrewritten-count: %d\nsource-pack-pending: %t\nsource-pack-reclaimed: %t\n",
		result.Executed, result.Before.BlobBytes, result.After.BlobBytes, result.RewrittenBytes, result.RewrittenCount,
		result.SourcePack != "" && !result.SourceGone, result.SourceGone); err != nil {
		return err
	}
	if result.DeferredReason != "" {
		if _, err := fmt.Fprintf(stdout, "deferred-reason: %s\n", result.DeferredReason); err != nil {
			return err
		}
	}
	if !result.NextRun.IsZero() {
		_, err = fmt.Fprintf(stdout, "next-run-utc: %s\n", result.NextRun.UTC().Format(time.RFC3339))
		return err
	}
	return nil
}

func runStorageCapacity(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("storage capacity [-C DIR] [--reserve-bytes N]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	reserveBytes := fs.Uint64("reserve-bytes", 0, "minimum free space to reserve (1–MaxInt64 bytes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("storage capacity takes no positional arguments")
	}
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "reserve-bytes" {
			provided = true
		}
	})
	if provided && (*reserveBytes == 0 || *reserveBytes > math.MaxInt64) {
		return errors.New("storage capacity --reserve-bytes must be between 1 and MaxInt64")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if provided {
		if err := w.SetCapacityPolicy(ctx, project.CapacityPolicy{ReserveBytes: *reserveBytes}); err != nil {
			return fmt.Errorf("set storage capacity policy: %w", err)
		}
	}
	policy, err := w.CapacityPolicy(ctx)
	if err != nil {
		return fmt.Errorf("read storage capacity policy: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "reserve-bytes: %d\n", policy.ReserveBytes)
	return err
}
