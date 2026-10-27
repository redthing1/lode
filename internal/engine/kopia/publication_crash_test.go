package kopia_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kopia/kopia/repo"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/project"
)

const publicationCrashExit = 73

type publicationCrashRoots struct {
	project.ContentRoots
	phase string
}

func (r publicationCrashRoots) Protect(ctx context.Context, key string, archive lodekopia.Archive) error {
	if r.phase == "before-protection" {
		os.Exit(publicationCrashExit)
	}
	if err := r.ContentRoots.Protect(ctx, key, archive); err != nil {
		return err
	}
	if r.phase == "after-protection" {
		os.Exit(publicationCrashExit)
	}
	return nil
}

// This subprocess deliberately exits without deferred cleanup. It owns a real
// project lock, SQLite connection, and native writer at the selected boundary.
func TestPublicationCrashHelper(t *testing.T) {
	phase := os.Getenv("LODE_TEST_PUBLICATION_CRASH")
	if phase == "" {
		t.Skip("subprocess helper")
	}
	switch phase {
	case "before-protection", "after-protection", "after-acceptance":
	default:
		t.Fatalf("unexpected crash phase %q", phase)
	}
	clock := &archiveClock{}
	ctx := context.WithValue(t.Context(), archiveClockKey{}, clock)
	repository, err := repo.Open(ctx, os.Getenv("LODE_TEST_REPOSITORY_CONFIG"), "temporary-metadata-proof-password", &repo.Options{TimeNowFunc: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := lodekopia.NewRoots(repository.(repo.DirectRepository))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := project.OpenMetadata(os.Getenv("LODE_TEST_METADATA_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := project.NewService(ctx, metadata, publicationCrashRoots{ContentRoots: roots, phase: phase}, project.Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("LODE_TEST_PUBLICATION_REQUEST"))
	if err != nil {
		t.Fatal(err)
	}
	var request project.PublishRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, request); err != nil {
		t.Fatal(err)
	}
	if phase != "after-acceptance" {
		t.Fatal("publication returned without reaching the requested crash boundary")
	}
	// The caller never receives the result. SQLite acceptance has committed,
	// but neither a reply nor normal service/engine/database cleanup occurs.
	os.Exit(publicationCrashExit)
}

func TestPublicationRecoversAfterProcessExit(t *testing.T) {
	for _, phase := range []string{"before-protection", "after-protection", "after-acceptance"} {
		t.Run(phase, func(t *testing.T) {
			f := newArchiveFixture(t)
			payload := archivePayload(256<<10, 301)
			root, archive := makeRootArchive(t, f, "asset.bin", payload)
			request := project.PublishRequest{
				OperationID: "crashed-operation", Branch: "main", Tree: archive,
				Author: "fixture", Message: "durable publication",
			}
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(f.dir, "publication.json")
			if err := os.WriteFile(requestPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(f.dir, "project.sqlite")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestPublicationCrashHelper$")
			command.Env = append(os.Environ(),
				"LODE_TEST_PUBLICATION_CRASH="+phase,
				"LODE_TEST_REPOSITORY_CONFIG="+f.connect("crash-process"),
				"LODE_TEST_METADATA_PATH="+databasePath,
				"LODE_TEST_PUBLICATION_REQUEST="+requestPath,
			)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != publicationCrashExit {
				t.Fatalf("child exit = %v; want %d\n%s", err, publicationCrashExit, output)
			}

			// New connections recover durable state. Acquiring the service also
			// proves the interrupted process no longer owns its filesystem lock.
			reopened := f.reader("recover-process").(repo.DirectRepository)
			defer reopened.Close(context.WithoutCancel(f.ctx))
			roots, err := lodekopia.NewRoots(reopened)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := project.OpenMetadata(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer metadata.Close()
			service, err := project.NewService(f.ctx, metadata, roots, project.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			operation, err := service.Operation(f.ctx, request.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := project.OperationPending
			if phase == "after-acceptance" {
				wantState = project.OperationAccepted
			}
			if operation.State != wantState {
				t.Fatalf("operation after exit = %s, want %s", operation.State, wantState)
			}
			head, err := service.Head(f.ctx, "main")
			if err != nil {
				t.Fatal(err)
			}
			if wantState == project.OperationPending && head != "" {
				t.Fatalf("unaccepted operation moved branch to %q", head)
			}
			if wantState == project.OperationAccepted && head != operation.CommitID {
				t.Fatalf("accepted operation lost branch update: %q != %q", head, operation.CommitID)
			}
			key := service.ID() + "/" + request.OperationID
			rootErr := roots.Check(f.ctx, key, archive)
			if phase == "before-protection" {
				if !errors.Is(rootErr, lodekopia.ErrRootNotFound) {
					t.Fatalf("root before protection = %v", rootErr)
				}
			} else if rootErr != nil {
				t.Fatalf("flushed protection missing after process exit: %v", rootErr)
			}
			if err := service.Recover(f.ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Resume(f.ctx, request.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.ID != operation.CommitID {
				t.Fatalf("recovery changed sealed candidate identity: %q != %q", recovered.ID, operation.CommitID)
			}
			replayed, err := service.Publish(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replayed, recovered) {
				t.Fatalf("accepted replay differs: %+v != %+v", replayed, recovered)
			}
			if head, err := service.Head(f.ctx, "main"); err != nil || head != recovered.ID {
				t.Fatalf("recovered branch = %q, %v", head, err)
			}
			if err := roots.Check(f.ctx, key, archive); err != nil {
				t.Fatal(err)
			}
			assertRootCount(t, f, key, 1)
			assertProtectedFile(t, f.ctx, reopened, root, "asset.bin", payload)
		})
	}
}
