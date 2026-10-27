// Package project coordinates shared history and recoverable publication.
package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

const metadataSchemaVersion = 5

// ErrMetadataNotFound means that no archived record exists for the requested key.
var ErrMetadataNotFound = errors.New("metadata record not found")

// ErrMetadataConflict means that an immutable metadata key already has different bytes.
var ErrMetadataConflict = errors.New("metadata immutable-byte conflict")

// Metadata stores project history, operation results, and opaque native tree
// records. It does not interpret the native directory or root representation.
type Metadata struct {
	db   *sql.DB
	path string
}

// OpenMetadata opens or creates a SQLite metadata database at the literal disk
// path. It does not accept SQLite DSNs or support the special :memory: name.
// Relative paths are resolved against the current working directory.
//
// The pool is limited to one open connection. SQLite's busy timeout and
// synchronous mode are configured in the DSN so they apply to every physical
// connection, including one opened after a connection is replaced.
func OpenMetadata(path string) (*Metadata, error) {
	return openMetadata(path, nil)
}

func openMetadata(path string, owner *flock.Flock) (*Metadata, error) {
	if path == "" {
		return nil, errors.New("open metadata: empty database path")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve metadata database path: %w", err)
	}
	if owner != nil && (!owner.Locked() || owner.Path() != absolutePath+".service-lock") {
		return nil, errors.New("open metadata requires its project owner's lock")
	}
	dsn, err := metadataDSN(absolutePath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open metadata database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	closeOnError := func(err error) (*Metadata, error) {
		_ = db.Close()
		return nil, err
	}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("connect metadata database: %w", err))
	}
	canonicalPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return closeOnError(fmt.Errorf("resolve metadata database symlinks: %w", err))
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return closeOnError(fmt.Errorf("read metadata schema version: %w", err))
	}
	if version != metadataSchemaVersion {
		migrationOwner := owner
		if migrationOwner == nil {
			migrationOwner, err = acquireProjectOwner(canonicalPath)
			if err != nil {
				return closeOnError(err)
			}
		}
		initErr := initializeMetadataSchema(ctx, db)
		var unlockErr error
		if owner == nil {
			unlockErr = migrationOwner.Close()
		}
		if initErr != nil {
			return closeOnError(initErr)
		}
		if unlockErr != nil {
			return closeOnError(fmt.Errorf("unlock metadata schema migration: %w", unlockErr))
		}
	}

	return &Metadata{db: db, path: canonicalPath}, nil
}

func metadataDSN(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve metadata database path: %w", err)
	}
	absPath = filepath.ToSlash(absPath)
	if runtime.GOOS == "windows" && len(absPath) >= 2 && absPath[1] == ':' {
		// SQLite's file URI syntax requires /C:/... for an absolute Windows
		// path rather than file://C:/..., where C: would be parsed as a host.
		absPath = "/" + absPath
	}
	query := url.Values{}
	query.Set("_busy_timeout", "5000")
	// EXTRA also syncs rollback-journal removal's directory entry.
	query.Set("_synchronous", "EXTRA")
	query.Set("_txlock", "immediate")
	return (&url.URL{
		Scheme:   "file",
		Path:     absPath,
		RawQuery: query.Encode(),
	}).String(), nil
}

// Close closes the metadata database. A Metadata value must not be used after Close.
func (m *Metadata) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}

// Import stores an immutable archive and returns its root-descriptor identity.
// Directory records are shared by repository namespace, format, and object ID;
// each root descriptor is kept separately because root attributes can differ
// even when the root directory object is the same.
func (m *Metadata) Import(ctx context.Context, namespace, format string, root []byte, directories map[string][]byte) (string, error) {
	if m == nil || m.db == nil {
		return "", errors.New("import metadata: database is not open")
	}
	if namespace == "" {
		return "", errors.New("import metadata: empty repository namespace")
	}
	if format == "" {
		return "", errors.New("import metadata: empty archive format")
	}
	if len(root) == 0 {
		return "", errors.New("import metadata: empty root descriptor")
	}
	keys := make([]string, 0, len(directories))
	for id := range directories {
		if id == "" {
			return "", errors.New("import metadata: empty directory object ID")
		}
		keys = append(keys, id)
	}
	sort.Strings(keys)

	rootID := rootDescriptorID(root)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin metadata import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, objectID := range keys {
		if err := putImmutable(ctx, tx,
			`INSERT INTO directory_manifests (namespace, format, object_id, data)
			 VALUES (?, ?, ?, ?) ON CONFLICT(namespace, format, object_id) DO NOTHING`,
			`SELECT data FROM directory_manifests WHERE namespace = ? AND format = ? AND object_id = ?`,
			[]any{namespace, format, objectID, nonNilBytes(directories[objectID])},
			[]any{namespace, format, objectID}, directories[objectID]); err != nil {
			return "", fmt.Errorf("import directory %q: %w", objectID, err)
		}
	}
	if err := putImmutable(ctx, tx,
		`INSERT INTO archive_roots (namespace, format, archive_id, descriptor)
		 VALUES (?, ?, ?, ?) ON CONFLICT(namespace, format, archive_id) DO NOTHING`,
		`SELECT descriptor FROM archive_roots WHERE namespace = ? AND format = ? AND archive_id = ?`,
		[]any{namespace, format, rootID, nonNilBytes(root)},
		[]any{namespace, format, rootID}, root); err != nil {
		return "", fmt.Errorf("import root descriptor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit metadata import: %w", err)
	}
	return rootID, nil
}

// Root retrieves a root descriptor by its archive ID returned from Import.
func (m *Metadata) Root(ctx context.Context, namespace, format, archiveID string) ([]byte, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("read metadata root: database is not open")
	}
	var data []byte
	err := m.db.QueryRowContext(ctx,
		`SELECT descriptor FROM archive_roots WHERE namespace = ? AND format = ? AND archive_id = ?`,
		namespace, format, archiveID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMetadataNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata root: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// Directory retrieves an opaque native directory record.
func (m *Metadata) Directory(ctx context.Context, namespace, format, objectID string) ([]byte, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("read metadata directory: database is not open")
	}
	var data []byte
	err := m.db.QueryRowContext(ctx,
		`SELECT data FROM directory_manifests WHERE namespace = ? AND format = ? AND object_id = ?`,
		namespace, format, objectID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMetadataNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata directory: %w", err)
	}
	return append([]byte(nil), data...), nil
}

func initializeMetadataSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read metadata schema version: %w", err)
	}
	if version == metadataSchemaVersion {
		return nil
	}
	if version == 4 {
		return migrateCollectionSchema(ctx, db)
	}
	if version != 0 && version != 1 && version != 2 && version != 3 {
		return fmt.Errorf("unsupported metadata schema version %d", version)
	}
	if version == 3 {
		if err := migrateRetentionSchema(ctx, db); err != nil {
			return err
		}
		return migrateCollectionSchema(ctx, db)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statements := []string{}
	if version == 0 {
		statements = append(statements,
			`CREATE TABLE IF NOT EXISTS directory_manifests (
			namespace TEXT NOT NULL,
			format TEXT NOT NULL,
			object_id TEXT NOT NULL,
			data BLOB NOT NULL,
			PRIMARY KEY (namespace, format, object_id)
		) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS archive_roots (
			namespace TEXT NOT NULL,
			format TEXT NOT NULL,
			archive_id TEXT NOT NULL,
			descriptor BLOB NOT NULL,
			PRIMARY KEY (namespace, format, archive_id)
		) WITHOUT ROWID`)
	}
	if version == 2 {
		statements = append(statements,
			`ALTER TABLE publication_ops RENAME TO publication_ops_v2`,
			publicationOpsTableSQL,
			`INSERT INTO publication_ops
				(id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done)
			 SELECT id, fingerprint, sealed_input, X'', candidate_commit_id, created_at, expires_at, state, observed_tip, release_done
			 FROM publication_ops_v2`,
			`DROP TABLE publication_ops_v2`,
		)
	}
	statements = append(statements,
		`CREATE TABLE IF NOT EXISTS project_binding (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			project_id TEXT NOT NULL,
			namespace TEXT NOT NULL
		) WITHOUT ROWID`)
	if version != 2 {
		statements = append(statements, publicationOpsTableSQL)
	}
	statements = append(statements,
		`CREATE TABLE IF NOT EXISTS file_facts (
			namespace TEXT NOT NULL,
			ref TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			size INTEGER NOT NULL CHECK (size >= 0),
			PRIMARY KEY (namespace, ref)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS commits (
			id TEXT PRIMARY KEY,
			data BLOB NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS branches (
			name TEXT PRIMARY KEY,
			tip TEXT NOT NULL
		) WITHOUT ROWID`,
		"PRAGMA user_version = 3")
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize metadata schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit metadata schema initialization: %w", err)
	}
	if err := migrateRetentionSchema(ctx, db); err != nil {
		return err
	}
	return migrateCollectionSchema(ctx, db)
}

func migrateCollectionSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read collection schema version: %w", err)
	}
	if version == metadataSchemaVersion {
		return nil
	}
	if version != 4 {
		return fmt.Errorf("cannot migrate collection schema from version %d", version)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin collection schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE TABLE capacity_policy (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			reserve_bytes INTEGER NOT NULL CHECK (reserve_bytes >= 0)
		) WITHOUT ROWID`,
		`INSERT INTO capacity_policy (singleton, reserve_bytes) VALUES (1, 1073741824)`,
		`CREATE TABLE compaction_state (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			source_pack_id TEXT NOT NULL,
			started_at TEXT NOT NULL,
			rewritten_at TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID`,
		`PRAGMA user_version = 5`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate collection schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit collection schema migration: %w", err)
	}
	return nil
}

const publicationOpsTableSQL = `CREATE TABLE publication_ops (
	id TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL,
	sealed_input BLOB NOT NULL,
	capture_input BLOB NOT NULL DEFAULT X'',
	candidate_commit_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('capturing', 'pending', 'accepted', 'stale', 'aborted', 'expired')),
	observed_tip TEXT NOT NULL,
	release_done INTEGER NOT NULL CHECK (release_done IN (0, 1))
) WITHOUT ROWID`

func putImmutable(ctx context.Context, tx *sql.Tx, insert, selectExisting string, insertArgs, selectArgs []any, expected []byte) error {
	result, err := tx.ExecContext(ctx, insert, insertArgs...)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 0 {
		return nil
	}
	var existing []byte
	if err := tx.QueryRowContext(ctx, selectExisting, selectArgs...).Scan(&existing); err != nil {
		return fmt.Errorf("read existing immutable record: %w", err)
	}
	if !bytes.Equal(existing, expected) {
		return ErrMetadataConflict
	}
	return nil
}

func rootDescriptorID(root []byte) string {
	sum := sha256.Sum256(root)
	return hex.EncodeToString(sum[:])
}

func nonNilBytes(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
}
