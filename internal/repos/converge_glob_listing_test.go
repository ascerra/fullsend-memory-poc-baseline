package repos

import (
	"context"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func globListingManifest(t *testing.T) *Manifest {
	t.Helper()
	var m Manifest
	require.NoError(t, yaml.Unmarshal([]byte(`
version: 1
github:
  mint_url: https://mint.example.com
  repos:
    - name: acme/*
`), &m))
	return &m
}

func acmeListing(names ...string) *forge.FakeClient {
	fc := forge.NewFakeClient()
	for _, n := range names {
		fc.Repos = append(fc.Repos, forge.Repository{Name: n, FullName: "acme/" + n})
	}
	return fc
}

// preflightOrgListings expands m's glob entries through a preflight read
// cache over fc, as the install preflights do, and returns its snapshot.
func preflightOrgListings(t *testing.T, m *Manifest, fc *forge.FakeClient) *OrgListings {
	t.Helper()
	cache := NewPreflightReadCache(newTestClientFactory(fc))
	_, err := m.ExpandGlobsFor(context.Background(), cache, nil)
	require.NoError(t, err)
	return cache.OrgListings()
}

// Repositories the live listing shows but the selection lacks are reported,
// unless the repository filter does not select them, and the selection is
// kept as it is.
func TestReconcileGlobSelection_ReportsNewRepos(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	preflight := preflightOrgListings(t, m, acmeListing("api"))
	selected, err := m.ExpandGlobsFor(ctx, newTestClientFactory(acmeListing("api")), nil)
	require.NoError(t, err)
	live := newTestClientFactory(acmeListing("api", "web", "docs"))
	const notice = "warning not installed: it appeared after this run's preflights evaluated the repository selection; run repos install again to install it"

	t.Run("no filter", func(t *testing.T) {
		progress := &progressLog{}
		kept := reconcileGlobSelection(ctx, m, live, preflight, nil, selected, progress.fn)
		assert.Equal(t, selected, kept)
		assert.Equal(t, []string{"acme/docs " + notice, "acme/web " + notice}, progress.all())
	})
	t.Run("filter", func(t *testing.T) {
		progress := &progressLog{}
		kept := reconcileGlobSelection(ctx, m, live, preflight, []string{"acme/api", "ACME/w*"}, selected, progress.fn)
		assert.Equal(t, selected, kept)
		assert.Equal(t, []string{"acme/web " + notice}, progress.all())
	})
	t.Run("nothing new", func(t *testing.T) {
		progress := &progressLog{}
		kept := reconcileGlobSelection(ctx, m, newTestClientFactory(acmeListing("api")), preflight, nil, selected, progress.fn)
		assert.Equal(t, selected, kept)
		assert.Empty(t, progress.all())
	})
}

// A repository the preflights' listing showed but the live listing no
// longer does (deleted, archived or transferred after the preflights) is
// dropped and reported, whether it is still glob-selected or the
// preflights gave it its own entry. An explicit entry neither listing shows,
// such as a fork, is kept.
func TestReconcileGlobSelection_DropsReposNoLongerListed(t *testing.T) {
	ctx := context.Background()
	const gone = "warning not installed: it is no longer listed in its org (deleted, archived or transferred) since this run's preflights evaluated the repository selection"
	m := globListingManifest(t)
	m.GitHub.Repos = append(m.GitHub.Repos, RepoEntry{Name: "acme/docs"}, RepoEntry{Name: "acme/forked"})
	preflight := preflightOrgListings(t, m, acmeListing("api", "web", "docs"))
	selected, err := m.ExpandGlobsFor(ctx, preflight.clients(newTestClientFactory(acmeListing())), nil)
	require.NoError(t, err)
	require.Len(t, selected, 4)
	live := acmeListing("api", "web")
	live.Repos[1].Archived = true

	progress := &progressLog{}
	kept := reconcileGlobSelection(ctx, m, newTestClientFactory(live), preflight, nil, selected, progress.fn)
	var names []string
	for _, rr := range kept {
		names = append(names, rr.Owner+"/"+rr.Repo)
	}
	assert.Equal(t, []string{"acme/api", "acme/forked"}, names)
	assert.Equal(t, []string{"acme/docs " + gone, "acme/web " + gone}, progress.all())
}

// A live listing that fails is reported and leaves the selection as it is.
func TestReconcileGlobSelection_ListingError(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	preflight := preflightOrgListings(t, m, acmeListing("api"))
	selected, err := m.ExpandGlobsFor(ctx, newTestClientFactory(acmeListing("api")), nil)
	require.NoError(t, err)
	failing := acmeListing()
	failing.Errors = map[string]error{"ListOrgRepos": assert.AnError}
	progress := &progressLog{}
	kept := reconcileGlobSelection(ctx, m, newTestClientFactory(failing), preflight, nil, selected, progress.fn)
	assert.Equal(t, selected, kept)
	lines := progress.all()
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], " warning Could not list glob entries' orgs again")
	assert.Contains(t, lines[0], assert.AnError.Error())
}

// Converge expands glob entries through GlobListing, so a repository only
// the live clients list is neither probed nor written, and is reported.
func TestConverge_GlobListingSelectsRepos(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	progress := &progressLog{}
	result, err := Converge(ctx, ConvergeConfig{
		Manifest:       m,
		DryRun:         true,
		MaxConcurrency: 1,
		GlobListing:    preflightOrgListings(t, m, acmeListing()),
	}, newTestClientFactory(acmeListing("web")), nil, progress.fn)
	require.NoError(t, err)
	assert.Empty(t, result.Results, "the selection the preflights saw is empty")
	lines := progress.all()
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "acme/web warning not installed: it appeared after")
}

// A glob org the snapshot does not hold fails convergence before any probe
// or write instead of being listed live.
func TestConverge_GlobListingMissingOrgFailsClosed(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	live := acmeListing("api")
	progress := &progressLog{}
	result, err := Converge(ctx, ConvergeConfig{
		Manifest:       m,
		MaxConcurrency: 1,
		GlobListing:    NewPreflightReadCache(newTestClientFactory(live)).OrgListings(),
	}, newTestClientFactory(live), nil, progress.fn)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), `org "acme" on github was not listed by this run's preflights`)
	assert.Empty(t, progress.all())
	assertNoForgeWrites(t, live)
}
