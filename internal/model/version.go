package model

import (
	"errors"
	"strings"
	"time"
)

type Reference struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func ValidReference(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if i == 0 || !strings.ContainsRune("-_./", r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func ShortVersion(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

type Version struct {
	ID          string    `json:"id"`
	Parent      string    `json:"parent,omitempty"`
	MergeParent string    `json:"merge_parent,omitempty"`
	Actor       string    `json:"actor"`
	Message     string    `json:"message"`
	Created     time.Time `json:"created"`
	Retained    bool      `json:"retained"`
}

type Save struct {
	ID      string          `json:"id"`
	Branch  string          `json:"branch"`
	Base    string          `json:"base"`
	Message string          `json:"message"`
	Changes []Entry         `json:"changes"`
	Merge   string          `json:"merge,omitempty"`
	Choices map[string]Side `json:"choices,omitempty"`
}

func (s Save) Validate() error {
	if !ValidReference(s.Branch) || len(s.Message) > 4096 {
		return errors.New("invalid branch or version message")
	}
	if s.Merge != "" && len(s.Changes) != 0 || s.Merge == "" && len(s.Choices) != 0 {
		return errors.New("save requires either file changes or a merge source and choices")
	}
	for name, side := range s.Choices {
		if !ValidPath(name) || side != Local && side != Incoming {
			return errors.New("invalid merge conflict choice")
		}
	}
	return ValidateChanges(s.Changes)
}
