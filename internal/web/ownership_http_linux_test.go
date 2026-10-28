//go:build linux

package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/remote"
)

func TestUsersCreateSameNamedPrivateProjectsAndScopedCLIAccess(t *testing.T) {
	ctx := context.Background()
	inst, origin, setupCode := testServer(t)
	owner := newBrowser(t, origin)
	wantStatus(t, owner.post("/setup", postFields("code", setupCode, "username", "owner", "password", "a long enough password")), http.StatusSeeOther)

	// The administrator's and ordinary user's projects may share a slug because
	// the canonical identity includes the owning username.
	wantStatus(t, owner.post("/projects/new", postFields("slug", "shared")), http.StatusSeeOther)
	adminProject, err := inst.Project(ctx, "owner", "shared")
	if err != nil {
		t.Fatal(err)
	}
	createdUser := owner.post("/-/admin/users", postFields("username", "sam"))
	wantStatus(t, createdUser, http.StatusOK)
	temporaryPassword := secretFromPage(t, createdUser.body)

	sam := newBrowser(t, origin)
	wantStatus(t, sam.post("/login", postFields("username", "sam", "password", temporaryPassword)), http.StatusSeeOther)
	wantStatus(t, sam.post("/settings/password", postFields("current", temporaryPassword, "password", "sam's project passphrase", "confirm", "sam's project passphrase")), http.StatusSeeOther)
	created := sam.post("/projects/new", postFields("slug", "shared"))
	wantStatus(t, created, http.StatusSeeOther)
	if created.header.Get("Location") != "/sam/shared" {
		t.Fatalf("ordinary user project location=%q", created.header.Get("Location"))
	}
	samProject, err := inst.Project(ctx, "sam", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if adminProject.ID == samProject.ID || adminProject.Path() != "owner/shared" || samProject.Path() != "sam/shared" {
		t.Fatalf("same-name project identities: owner=%+v sam=%+v", adminProject, samProject)
	}

	wantStatus(t, sam.get("/sam/shared"), http.StatusOK)
	wantStatus(t, owner.get("/sam/shared"), http.StatusOK) // administrators retain instance-wide access
	private := sam.get("/owner/shared")
	wantStatus(t, private, http.StatusForbidden)
	projectList := sam.get("/")
	wantStatus(t, projectList, http.StatusOK)
	if !strings.Contains(projectList.body, "sam/shared") || strings.Contains(projectList.body, "owner/shared") {
		t.Fatalf("project listing leaked another owner's project: %s", projectList.body)
	}

	tokenPage := sam.post("/settings/tokens", postFields("name", "sam-shared", "project", samProject.ID, "role", "writer", "days", "90"))
	wantStatus(t, tokenPage, http.StatusOK)
	token := secretFromPage(t, tokenPage.body)
	client, err := remote.New(origin+"/sam/shared", token, samProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Info(ctx)
	if err != nil || info.ID != samProject.ID {
		t.Fatalf("scoped CLI identity via owner/project URL: %+v %v", info, err)
	}
	crossOwner, err := remote.New(origin+"/owner/shared", token, adminProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crossOwner.Info(ctx); err == nil {
		t.Fatal("user-scoped CLI token accessed another owner's same-named project")
	}
}
