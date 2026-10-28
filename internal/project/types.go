// Package project owns shared history and publication decisions.
package project

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/model"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrStale             = errors.New("branch changed; update the workspace before saving")
	ErrCancelled         = errors.New("operation was cancelled")
	ErrOperationConflict = errors.New("operation ID is bound to a different request")
	ErrInvalid           = errors.New("invalid request")
	ErrAlreadyAccepted   = errors.New("operation was already accepted; resume it")
	ErrExists            = errors.New("name already exists")
	ErrAlreadyMerged     = errors.New("source version is already merged")
	ErrExpired           = errors.New("version contents have expired")
	ErrCleanupChanged    = errors.New("history or protection changed; preview cleanup again")
	ErrReferenceChanged  = errors.New("reference changed; list references again")
	ErrOperationOwner    = errors.New("operation is owned by another actor")
)

// Actor is the authenticated, stable identity supplied by the instance layer.
// Name is a display snapshot; ID is the durable ownership key.
type Actor struct{ ID, Name string }

type Commit struct {
	ID       string    `json:"id"`
	ActorID  string    `json:"actor_id,omitempty"`
	Parents  []string  `json:"parents"`
	Author   string    `json:"author"`
	Message  string    `json:"message"`
	Created  time.Time `json:"created"`
	Retained bool      `json:"retained"`
}

type Request struct {
	ID           string         `json:"id"`
	Branch       string         `json:"branch"`
	Base         string         `json:"base"`
	Author       string         `json:"author"`
	Message      string         `json:"message"`
	Changes      []model.Entry  `json:"changes"`
	SecondParent string         `json:"second_parent,omitempty"`
	MergeBase    string         `json:"merge_base,omitempty"`
	Choices      []model.Choice `json:"choices,omitempty"`
	actorID      string         `json:"-"`
}

type MergeBaseConflict struct{ Bases []string }

func (e *MergeBaseConflict) Error() string {
	return "multiple best merge bases: " + strings.Join(e.Bases, ", ")
}

type Branch struct {
	Name string `json:"name"`
	Tip  string `json:"tip"`
}

type Tag struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type RetentionVersion struct {
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	Retained bool      `json:"retained"`
	Branches int       `json:"branches"`
	Tags     int       `json:"tags"`
	Recent   int       `json:"recent"`
}

func (v RetentionVersion) Protected() bool {
	return v.Branches > 0 || v.Tags > 0 || v.Recent > 0
}

type CleanupPlan struct {
	Keep     int                `json:"keep"`
	Token    string             `json:"token"`
	Versions []RetentionVersion `json:"versions"`
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func ValidID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
}
