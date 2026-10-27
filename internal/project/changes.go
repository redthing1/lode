package project

import (
	"github.com/redthing1/lode/internal/model"
)

// Change is one explicit whole-entry change. File changes bind exact uploaded
// bytes by SHA-256 and size. Directory removal never implies recursive deletion.
type Change struct {
	Path   string
	Kind   model.EntryKind
	SHA256 string
	Size   int64
}

// ChangeRequest seals sparse changes against one complete expected branch base.
// The server constructs the complete result, preserving all omitted entries.
type ChangeRequest struct {
	OperationID string
	Branch      string
	Base        string
	Author      string
	Message     string
	Changes     []Change
}

// Node describes one logical entry without exposing native storage references.
// File digests support local comparison without a repository-wide native index.
type Node struct {
	Path   string
	Kind   model.EntryKind
	SHA256 string
	Size   int64
}

// SelectionRule is the portable representation of a model selection rule.
type SelectionRule struct {
	Path     string
	Included bool
}
