package cli

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// changingOrgListingClient lists, on its i-th ListOrgRepos call, only the
// repositories named in listings[i], and on every call past the end of
// listings those named in its last element, like an org in which
// repositories are created or removed while `repos install` runs.
//
// `repos install` lists a glob org three times: the GitLab trigger-account
// containment selection, the preflight read cache's single listing, and
// convergence's live check for repositories created since the preflights.
type changingOrgListingClient struct {
	*forge.FakeClient

	listings [][]string

	mu    sync.Mutex
	calls int
}

func (c *changingOrgListingClient) ListOrgRepos(ctx context.Context, org string, includePrivate bool) ([]forge.Repository, error) {
	c.mu.Lock()
	names := c.listings[min(c.calls, len(c.listings)-1)]
	c.calls++
	c.mu.Unlock()
	listed, err := c.FakeClient.ListOrgRepos(ctx, org, includePrivate)
	if err != nil {
		return listed, err
	}
	var kept []forge.Repository
	for _, r := range listed {
		for _, name := range names {
			if r.FullName == name {
				kept = append(kept, r)
			}
		}
	}
	return kept, nil
}

// Convergence selects the glob-covered repositories the preflights
// evaluated. A repository created after them is not converged, gets no
// trace signing key and is reported so the operator can run install again,
// in dry runs and normal installs alike.
func TestRunReposInstall_ConvergesPreflightGlobSelection(t *testing.T) {
	manifest := strings.Replace(githubUnmanagedManifestYAML, "acme/api", "acme/*", 1)
	const notice = "[acme/web] not installed: it appeared after this run's preflights evaluated the repository selection; run repos install again to install it"
	for _, dryRun := range []bool{false, true} {
		name := "install"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			path := writeTestManifest(t, manifest)
			fc := newInstallFakeClient("acme/api", "acme/web")
			client := &changingOrgListingClient{FakeClient: fc, listings: [][]string{
				{"acme/api"}, {"acme/api"}, {"acme/api", "acme/web"},
			}}
			opts := githubManagedInstallOpts(path, fc)
			opts.testClient = client
			opts.dryRun = dryRun
			var err error
			out := captureStdout(t, func() { err = runReposInstall(context.Background(), opts) })
			require.NoError(t, err, out)

			assert.Contains(t, out, notice)
			assert.Equal(t, 3, client.calls, "convergence lists the org again to find new repositories")
			assert.NotContains(t, out, "[acme/web] Would", "acme/web is not previewed")
			assert.Empty(t, fc.FileContents[overlayPath("acme/web")], "acme/web gets no scaffold commit")
			assert.Empty(t, writtenTraceSeed(fc, "acme/web"), "acme/web gets no trace signing key")
			assert.NotContains(t, mustReadFile(t, path), "acme/web", "repos.yaml records nothing for acme/web")
			if dryRun {
				assertNoInstallWrites(t, fc)
				assert.Contains(t, out, "[acme/api] Would")
				return
			}
			assert.Contains(t, string(fc.FileContents[overlayPath("acme/api")]), "fullsend-ai/fullsend", "acme/api converges")
			requireTraceKeyWritten(t, fc, "acme/api", out)
		})
	}
}

// A repository listed by the preflights but gone from the live listing
// (deleted, archived or transferred in between) is reported and not probed
// or written, and the others still converge.
func TestRunReposInstall_GlobRepoRemovedAfterPreflightsIsSkipped(t *testing.T) {
	manifest := strings.Replace(githubUnmanagedManifestYAML, "acme/api", "acme/*", 1)
	path := writeTestManifest(t, manifest)
	fc := newInstallFakeClient("acme/api", "acme/web")
	client := &changingOrgListingClient{FakeClient: fc, listings: [][]string{
		{"acme/api", "acme/web"}, {"acme/api", "acme/web"}, {"acme/api"},
	}}
	opts := githubManagedInstallOpts(path, fc)
	opts.testClient = client
	var err error
	out := captureStdout(t, func() { err = runReposInstall(context.Background(), opts) })
	require.NoError(t, err, out)
	assert.NotContains(t, out, "not installed: it appeared after")
	assert.Contains(t, out, "[acme/web] not installed: it is no longer listed in its org (deleted, archived or transferred)")
	assert.Empty(t, fc.FileContents[overlayPath("acme/web")], "acme/web gets no scaffold commit")
	assert.Empty(t, writtenTraceSeed(fc, "acme/web"), "acme/web gets no trace signing key")
	assert.Contains(t, string(fc.FileContents[overlayPath("acme/api")]), "fullsend-ai/fullsend", "acme/api converges")
	requireTraceKeyWritten(t, fc, "acme/api", out)
}
