package project

import (
	"errors"
	"time"
)

var (
	ErrPayloadExpired    = errors.New("version contents were deliberately expired")
	ErrTagNotFound       = errors.New("tag not found")
	ErrTagExists         = errors.New("tag already protects another commit")
	ErrReadLeaseConflict = errors.New("read hold ID is bound to another version")
)

// RetentionPolicy keeps a first-parent tail per branch and recently created
// versions. Tips, tags, and active operations are always protected.
// Both optional history rules can be disabled with zero.
type RetentionPolicy struct {
	KeepLast int
	KeepDays int
}

func DefaultRetentionPolicy() RetentionPolicy { return RetentionPolicy{KeepLast: 10, KeepDays: 7} }

type Tag struct {
	Name     string
	CommitID string
}

// ReadLease protects one retained version for a bounded interval. Renewing a
// hold requires another successful request; abandoned holds expire naturally.
type ReadLease struct {
	ID        string
	CommitID  string
	ExpiresAt time.Time
	Lifetime  time.Duration
}

// Availability reports retention intent separately from native release work.
// Retained is not an assertion that a full payload integrity audit has run.
type Availability struct {
	CommitID       string
	State          string
	ExpiredAt      time.Time
	ReleasePending bool
	Reasons        []string
}

type RetentionCandidate struct {
	CommitID  string
	CreatedAt time.Time
}
type RetentionPreview struct {
	Policy          RetentionPolicy
	EvaluatedAt     time.Time
	Candidates      []RetentionCandidate
	Next            string
	TotalCandidates int
}
type ExpirationResult struct {
	Expired   []string
	Protected []Availability
}

// StorageStats separates repository blob lengths, indexed live bytes, and
// capacity shared with other users of the underlying volume.
type StorageStats struct {
	BlobBytes, PackBytes, IndexedLiveBytes, UnreferencedPackBytes int64
	BlobCount, PackCount                                          int64
	CapacityBytes, FreeBytes                                      uint64
	CapacityKnown                                                 bool
}

type payloadRecord struct {
	CommitID     string
	ExpiredAt    time.Time
	Released     bool
	ProtectUntil time.Time
}

type retentionSnapshot struct {
	Commits    map[string]Commit
	Payloads   map[string]payloadRecord
	Branches   []Branch
	Tags       []Tag
	Leases     []ReadLease
	Operations []publicationRecord
	Policy     RetentionPolicy
}
