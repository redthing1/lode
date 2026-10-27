package project

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestMetadata(t *testing.T) (*Metadata, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("OpenMetadata() error = %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return metadata, path
}

func TestMetadataImportIsIdempotentAndSharesDirectoryRows(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()
	dir := []byte{0, 1, 0xff, 'x'}
	rootOne := []byte(`{"root":"one"}`)
	rootTwo := []byte(`{"root":"two"}`)

	idOne, err := metadata.Import(ctx, "repo", "format-v1", rootOne, map[string][]byte{"dir-a": dir})
	if err != nil {
		t.Fatalf("first Import() error = %v", err)
	}
	duplicateID, err := metadata.Import(ctx, "repo", "format-v1", rootOne, map[string][]byte{"dir-a": dir})
	if err != nil {
		t.Fatalf("duplicate Import() error = %v", err)
	}
	if idOne != duplicateID {
		t.Fatalf("duplicate archive IDs differ: %q != %q", idOne, duplicateID)
	}
	idTwo, err := metadata.Import(ctx, "repo", "format-v1", rootTwo, map[string][]byte{"dir-a": dir})
	if err != nil {
		t.Fatalf("second root Import() error = %v", err)
	}
	if idOne == idTwo {
		t.Fatalf("different root descriptors share archive ID %q", idOne)
	}

	var directoryCount, rootCount int
	if err := metadata.db.QueryRowContext(ctx, "SELECT count(*) FROM directory_manifests").Scan(&directoryCount); err != nil {
		t.Fatal(err)
	}
	if err := metadata.db.QueryRowContext(ctx, "SELECT count(*) FROM archive_roots").Scan(&rootCount); err != nil {
		t.Fatal(err)
	}
	if directoryCount != 1 || rootCount != 2 {
		t.Fatalf("stored rows = %d directories, %d roots; want 1, 2", directoryCount, rootCount)
	}
}

func TestMetadataKeysAreScopedByNamespaceAndFormat(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()
	for _, test := range []struct {
		namespace string
		format    string
		data      []byte
	}{
		{"repo-a", "format-v1", []byte("a")},
		{"repo-b", "format-v1", []byte("b")},
		{"repo-a", "format-v2", []byte("c")},
	} {
		if _, err := metadata.Import(ctx, test.namespace, test.format, []byte("root"), map[string][]byte{"same-id": test.data}); err != nil {
			t.Fatalf("Import(%q, %q) error = %v", test.namespace, test.format, err)
		}
	}
	for _, test := range []struct {
		namespace string
		format    string
		want      []byte
	}{
		{"repo-a", "format-v1", []byte("a")},
		{"repo-b", "format-v1", []byte("b")},
		{"repo-a", "format-v2", []byte("c")},
	} {
		got, err := metadata.Directory(ctx, test.namespace, test.format, "same-id")
		if err != nil {
			t.Fatalf("Directory(%q, %q) error = %v", test.namespace, test.format, err)
		}
		if !bytes.Equal(got, test.want) {
			t.Errorf("Directory(%q, %q) = %q, want %q", test.namespace, test.format, got, test.want)
		}
	}
}

func TestMetadataDirectoryCollisionRollsBackWholeImport(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()
	if _, err := metadata.Import(ctx, "repo", "format-v1", []byte("existing-root"), map[string][]byte{"z-existing": []byte("original")}); err != nil {
		t.Fatal(err)
	}

	// "a-new" sorts before "z-existing", so its insert must be rolled back
	// when the later immutable-key collision is discovered.
	_, err := metadata.Import(ctx, "repo", "format-v1", []byte("candidate-root"), map[string][]byte{
		"a-new":      []byte("must roll back"),
		"z-existing": []byte("different"),
	})
	if !errors.Is(err, ErrMetadataConflict) {
		t.Fatalf("Import() error = %v, want ErrMetadataConflict", err)
	}
	if _, err := metadata.Directory(ctx, "repo", "format-v1", "a-new"); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("Directory(a-new) error = %v, want ErrMetadataNotFound", err)
	}
	if _, err := metadata.Root(ctx, "repo", "format-v1", rootDescriptorID([]byte("candidate-root"))); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("Root(candidate) error = %v, want ErrMetadataNotFound", err)
	}
}

func TestMetadataRestartLookupAndReturnedCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	root := []byte("root")
	directory := []byte{0, 0xff, 3}
	rootID, err := metadata.Import(context.Background(), "repo", "format-v1", root, map[string][]byte{"dir": directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	gotRoot, err := metadata.Root(context.Background(), "repo", "format-v1", rootID)
	if err != nil {
		t.Fatal(err)
	}
	gotDirectory, err := metadata.Directory(context.Background(), "repo", "format-v1", "dir")
	if err != nil {
		t.Fatal(err)
	}
	gotRoot[0] = 'X'
	gotDirectory[0] = 99
	gotRoot, err = metadata.Root(context.Background(), "repo", "format-v1", rootID)
	if err != nil || !bytes.Equal(gotRoot, root) {
		t.Fatalf("Root() after mutating returned bytes = %q, %v", gotRoot, err)
	}
	gotDirectory, err = metadata.Directory(context.Background(), "repo", "format-v1", "dir")
	if err != nil || !bytes.Equal(gotDirectory, directory) {
		t.Fatalf("Directory() after mutating returned bytes = %v, %v", gotDirectory, err)
	}
}

func TestMetadataMissingAndInvalidInputs(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()
	if _, err := metadata.Root(ctx, "repo", "format-v1", "missing"); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("Root(missing) error = %v, want ErrMetadataNotFound", err)
	}
	if _, err := metadata.Directory(ctx, "repo", "format-v1", "missing"); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("Directory(missing) error = %v, want ErrMetadataNotFound", err)
	}
	for _, test := range []struct {
		name      string
		namespace string
		format    string
		root      []byte
		dirs      map[string][]byte
	}{
		{"empty namespace", "", "format-v1", []byte("root"), nil},
		{"empty format", "repo", "", []byte("root"), nil},
		{"empty root", "repo", "format-v1", nil, nil},
		{"empty object ID", "repo", "format-v1", []byte("root"), map[string][]byte{"": []byte("dir")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := metadata.Import(ctx, test.namespace, test.format, test.root, test.dirs); err == nil {
				t.Fatal("Import() succeeded with invalid input")
			}
		})
	}
}

func TestMetadataContextAndClosedDatabaseErrors(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := metadata.Import(ctx, "repo", "format-v1", []byte("root"), map[string][]byte{"dir": []byte("bytes")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Import(canceled context) error = %v, want context.Canceled", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.Directory(context.Background(), "repo", "format-v1", "dir"); err == nil || errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("Directory() on closed DB error = %v, want database-closed error", err)
	}
}

func TestMetadataDSNPreservesLiteralQueryAndFragmentCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata ?# literal.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("OpenMetadata() error = %v", err)
	}
	if _, err := metadata.Import(context.Background(), "repo", "format-v1", []byte("root"), nil); err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat literal database path: %v", err)
	}
}

func TestMetadataPragmasApplyToReplacementConnections(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()

	checkPragmas := func(conn *sql.Conn) {
		t.Helper()
		var busyTimeout, synchronous int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if busyTimeout != 5000 || synchronous != 3 {
			t.Fatalf("connection pragmas = busy_timeout %d, synchronous %d; want 5000, 3", busyTimeout, synchronous)
		}
	}

	first, err := metadata.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPragmas(first)
	metadata.db.SetMaxIdleConns(0)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := metadata.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPragmas(second)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataRejectsFutureSchemaWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future-schema.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenMetadata(path); err == nil || !strings.Contains(err.Error(), "unsupported metadata schema version 6") {
		t.Fatalf("OpenMetadata() error = %v, want unsupported schema version", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, tableCount int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if version != 6 || tableCount != 0 {
		t.Fatalf("database after rejected open: user_version=%d, tables=%d; want 6, 0", version, tableCount)
	}
}
