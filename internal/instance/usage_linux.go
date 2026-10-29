package instance

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

type StorageSample struct {
	model.StorageUsage
	Measured time.Time
	Failed   bool
}

func (s StorageSample) Known() bool { return !s.Measured.IsZero() }

type ProjectUsage struct {
	Project
	Storage StorageSample
}

type AccountUsage struct {
	Projects []ProjectUsage
	Total    model.StorageUsage
	Complete bool
}

type usageEntry struct {
	StorageSample
	checked time.Time
}

type usageCache struct {
	mu      sync.Mutex
	entries map[string]usageEntry
	scan    chan struct{}
}

// CachedUsage never starts IO. The caller has already authorized the project.
func (s *Store) CachedUsage(id string) StorageSample {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	return s.usage.entries[id].StorageSample
}

func (s *Store) ProjectUsage(ctx context.Context, actor Principal, owner, name string) (StorageSample, error) {
	p, err := s.Project(ctx, actor, owner, name, Reader)
	if err != nil {
		return StorageSample{}, err
	}
	return s.measureUsage(ctx, p.ID), nil
}

// AccountUsage attributes storage to the owner, including pending deletions.
func (s *Store) AccountUsage(ctx context.Context, actor Principal, refresh bool) (AccountUsage, error) {
	if actor.Token {
		return AccountUsage{}, ErrAccess
	}
	user, err := currentUser(ctx, s.db, actor.User)
	if err != nil {
		return AccountUsage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,u.name,p.name,p.state FROM projects p JOIN users u ON u.id=p.owner WHERE p.owner=? ORDER BY p.name`, user.ID)
	if err != nil {
		return AccountUsage{}, err
	}
	result := AccountUsage{Complete: true}
	for rows.Next() {
		p := ProjectUsage{Project: Project{Role: Maintainer}}
		if err := rows.Scan(&p.ID, &p.Owner, &p.Name, &p.State); err != nil {
			rows.Close()
			return AccountUsage{}, err
		}
		result.Projects = append(result.Projects, p)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return AccountUsage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for i := range result.Projects {
		p := &result.Projects[i]
		p.Storage = s.CachedUsage(p.ID)
		if refresh {
			p.Storage = s.measureUsage(ctx, p.ID)
		}
		result.Complete = result.Complete && p.Storage.Known()
		result.Total.Bytes += p.Storage.Bytes
		result.Total.Allocated += p.Storage.Allocated
	}
	sort.SliceStable(result.Projects, func(i, j int) bool {
		return result.Projects[i].Storage.Allocated > result.Projects[j].Storage.Allocated
	})
	return result, nil
}

func (s *Store) measureUsage(ctx context.Context, id string) StorageSample {
	c := &s.usage
	c.mu.Lock()
	if c.scan == nil {
		c.scan = make(chan struct{}, 1)
	}
	entry := c.entries[id]
	c.mu.Unlock()
	if time.Since(entry.checked) < time.Minute {
		return entry.StorageSample
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// One metadata scan per instance; concurrent misses reuse the first result.
	select {
	case c.scan <- struct{}{}:
		defer func() { <-c.scan }()
	case <-ctx.Done():
		entry.Failed = true
		return entry.StorageSample
	}
	c.mu.Lock()
	entry = c.entries[id]
	c.mu.Unlock()
	if time.Since(entry.checked) < time.Minute {
		return entry.StorageSample
	}
	started := time.Now()
	usage, err := content.MeasureUsage(ctx, filepath.Join(s.directory, "projects", id, "repository"))
	entry.checked, entry.Failed = time.Now(), err != nil
	if err == nil {
		entry.StorageSample = StorageSample{StorageUsage: usage, Measured: entry.checked}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// A collection may have supplied a newer, consistent result during the scan.
	if newer := c.entries[id]; newer.checked.After(started) {
		return newer.StorageSample
	}
	c.store(id, entry)
	return entry.StorageSample
}

// RecordUsage reuses the consistent measurement already made by collection.
func (s *Store) RecordUsage(id string, usage model.StorageUsage) StorageSample {
	sample := StorageSample{StorageUsage: usage, Measured: time.Now()}
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	s.usage.store(id, usageEntry{StorageSample: sample, checked: sample.Measured})
	return sample
}

func (c *usageCache) store(id string, entry usageEntry) {
	if c.entries == nil {
		c.entries = map[string]usageEntry{}
	}
	if _, exists := c.entries[id]; !exists && len(c.entries) >= 512 {
		oldest := ""
		for key, value := range c.entries {
			if oldest == "" || c.entries[oldest].checked.After(value.checked) {
				oldest = key
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[id] = entry
}
