package project

import (
	"context"
	"errors"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
)

var (
	ErrOperationNotFound  = errors.New("publication operation not found")
	ErrCommitNotFound     = errors.New("commit not found")
	ErrOperationConflict  = errors.New("operation ID is already bound to different inputs")
	ErrStaleHead          = errors.New("branch tip no longer matches the publication base")
	ErrOperationAborted   = errors.New("publication operation aborted")
	ErrOperationExpired   = errors.New("publication operation expired")
	ErrAlreadyAccepted    = errors.New("publication operation already accepted")
	ErrRepositoryMismatch = errors.New("project database belongs to another content repository")
	ErrProjectBusy        = errors.New("another project service owns this database")
	ErrServiceClosed      = errors.New("project service is closed")
	ErrUploadRequired     = errors.New("operation needs its sealed file contents uploaded")
)

// TreeRef identifies archived metadata. Payload availability is a separate fact.
type TreeRef struct {
	Format string
	ID     string
}

// Commit is an accepted immutable complete version of the project.
type Commit struct {
	ID        string
	Parents   []string
	Tree      TreeRef
	Author    string
	Message   string
	CreatedAt time.Time
}

type OperationState string

const (
	OperationCapturing OperationState = "capturing"
	OperationPending   OperationState = "pending"
	OperationAccepted  OperationState = "accepted"
	OperationStale     OperationState = "stale"
	OperationAborted   OperationState = "aborted"
	OperationExpired   OperationState = "expired"
)

// Operation exposes a durable publication outcome. CommitID names accepted
// history only when State is OperationAccepted.
type Operation struct {
	ID          string
	State       OperationState
	CommitID    string
	Branch      string
	Base        string
	ObservedTip string
	ExpiresAt   time.Time
}

// PublishRequest supplies a server-captured complete tree. Base must be the
// expected current branch tip; an empty base creates the first commit on an
// absent branch. OperationID binds these inputs for all subsequent retries.
type PublishRequest struct {
	OperationID string
	Branch      string
	Base        string
	Tree        kopia.Archive
	Author      string
	Message     string
}

// ContentRoots is the native storage boundary used during publication and
// recovery. Protect must flush complete protection before returning success.
// The concrete implementation is Kopia; substitution supports fault testing.
type ContentRoots interface {
	Namespace() string
	Validate(context.Context, kopia.Archive) error
	Protect(context.Context, string, kopia.Archive) error
	Release(context.Context, string) error
}

// Options controls pending-operation lifetime and supplies a clock for recovery.
// Zero OperationTTL uses 24 hours; accepted results do not expire with this lease.
type Options struct {
	OperationTTL time.Duration
	Now          func() time.Time
	Objects      *kopia.Objects
	Maintenance  ContentMaintenance
	ReadTTL      time.Duration
}

// publicationInput is the sealed logical request. Directory streams are
// addressed by the native root descriptor and stored separately in SQLite.
type publicationInput struct {
	Branch      string
	Base        string
	MergeParent string `json:",omitempty"`
	Tree        TreeRef
	Author      string
	Message     string
}

type publicationRecord struct {
	ID           string
	Fingerprint  string
	Input        publicationInput
	CommitID     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	State        OperationState
	ObservedTip  string
	ReleaseDone  bool
	CaptureInput []byte
}

func (p publicationRecord) operation() Operation {
	return Operation{
		ID: p.ID, State: p.State, CommitID: p.CommitID,
		Branch: p.Input.Branch, Base: p.Input.Base,
		ObservedTip: p.ObservedTip, ExpiresAt: p.ExpiresAt,
	}
}
