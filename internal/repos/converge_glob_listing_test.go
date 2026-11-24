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
		kept, dropped, err := reconcileGlobSelection(ctx, m, live, preflight, nil, selected, progress.fn)
		require.NoError(t, err)
		assert.Equal(t, selected, kept)
		assert.Empty(t, dropped)
		assert.Equal(t, []string{"acme/docs " + notice, "acme/web " + notice}, progress.all())
	})
	t.Run("filter", func(t *testing.T) {
		progress := &progressLog{}
		kept, dropped, err := reconcileGlobSelection(ctx, m, live, preflight, []string{"acme/api", "ACME/w*"}, selected, progress.fn)
		require.NoError(t, err)
		assert.Equal(t, selected, kept)
		assert.Empty(t, dropped)
		assert.Equal(t, []string{"acme/web " + notice}, progress.all())
	})
	t.Run("nothing new", func(t *testing.T) {
		progress := &progressLog{}
		kept, dropped, err := reconcileGlobSelection(ctx, m, newTestClientFactory(acmeListing("api")), preflight, nil, selected, progress.fn)
		require.NoError(t, err)
		assert.Equal(t, selected, kept)
		assert.Empty(t, dropped)
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
	kept, dropped, err := reconcileGlobSelection(ctx, m, newTestClientFactory(live), preflight, nil, selected, progress.fn)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api", "acme/forked"}, repoNames(kept))
	assert.Equal(t, []string{"acme/docs", "acme/web"}, repoNames(dropped))
	assert.Equal(t, []string{"acme/docs " + gone, "acme/web " + gone}, progress.all())
}

func repoNames(rrs []ResolvedRepo) []string {
	var names []string
	for _, rr := range rrs {
		names = append(names, rr.Owner+"/"+rr.Repo)
	}
	return names
}

// Org names are compared case-insensitively: an explicit entry whose owner
// is spelled differently from the glob's org, or a glob whose org is spelled
// differently from the explicit entry, is still checked against the listing
// that glob read, and dropped when the live listing no longer shows it.
func TestReconcileGlobSelection_MixedCaseOwner(t *testing.T) {
	ctx := context.Background()
	const gone = "warning not installed: it is no longer listed in its org (deleted, archived or transferred) since this run's preflights evaluated the repository selection"
	for name, entries := range map[string][]RepoEntry{
		"explicit entry":  {{Name: "acme/*"}, {Name: "Acme/docs"}},
		"glob org":        {{Name: "ACME/*"}, {Name: "acme/docs"}},
		"both mixed-case": {{Name: "aCmE/*"}, {Name: "AcMe/docs"}, {Name: "acme/forked"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := globListingManifest(t)
			m.GitHub.Repos = entries
			preflight := preflightOrgListings(t, m, acmeListing("api", "docs"))
			selected, err := m.ExpandGlobsFor(ctx, preflight.clients(newTestClientFactory(acmeListing())), nil)
			require.NoError(t, err)

			progress := &progressLog{}
			kept, dropped, err := reconcileGlobSelection(ctx, m, newTestClientFactory(acmeListing("api")), preflight, nil, selected, progress.fn)
			require.NoError(t, err)
			require.Len(t, dropped, 1)
			assert.Equal(t, "docs", dropped[0].Repo)
			for _, rr := range kept {
				assert.NotEqual(t, "docs", rr.Repo)
			}
			lines := progress.all()
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], "/docs "+gone)
		})
	}
}

// createAfterFirstListClient counts the org listings that reach the wrapped
// fake and, right after the first one, adds the created repository to the
// org, so a second listing of the org differs from the first.
type createAfterFirstListClient struct {
	*forge.FakeClient

	created forge.Repository
	lists   []string
}

func (c *createAfterFirstListClient) ListOrgRepos(ctx context.Context, org string, includePrivate bool) ([]forge.Repository, error) {
	repos, err := c.FakeClient.ListOrgRepos(ctx, org, includePrivate)
	c.lists = append(c.lists, org)
	if len(c.lists) == 1 {
		c.Repos = append(c.Repos, c.created)
	}
	return repos, err
}

// Glob entries whose orgs differ only in case read one listing through the
// preflight cache, so the snapshot never has to choose between two listings
// read at different moments. A repository created after that one listing is
// left out of the selection and reported, whichever glob it matches.
func TestReconcileGlobSelection_CaseVariantGlobOrgs(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	m.GitHub.Repos = []RepoEntry{{Name: "Acme/a*"}, {Name: "acme/b*"}}
	fc := &createAfterFirstListClient{
		FakeClient: acmeListing("api"),
		created:    forge.Repository{Name: "billing", FullName: "acme/billing"},
	}
	cache := NewPreflightReadCache(newTestClientFactory(fc))
	// Two preflights expand the same manifest through the shared cache.
	for range 2 {
		_, err := m.ExpandGlobsFor(ctx, cache, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"Acme"}, fc.lists, "the preflights list the org once, whatever its case")
	preflight := cache.OrgListings()

	selected, err := m.ExpandGlobsFor(ctx, preflight.clients(newTestClientFactory(fc)), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Acme/api"}, repoNames(selected))

	progress := &progressLog{}
	kept, dropped, err := reconcileGlobSelection(ctx, m, newTestClientFactory(fc), preflight, nil, selected, progress.fn)
	require.NoError(t, err)
	assert.Equal(t, selected, kept)
	assert.Empty(t, dropped)
	assert.Equal(t, []string{"acme/billing warning not installed: it appeared after this run's preflights evaluated the repository selection; run repos install again to install it"}, progress.all())
}

// A live listing that fails stops reconciliation with the error, so
// convergence cannot probe or write a repository it can no longer tell was
// removed since the preflights.
func TestReconcileGlobSelection_ListingError(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	preflight := preflightOrgListings(t, m, acmeListing("api"))
	selected, err := m.ExpandGlobsFor(ctx, newTestClientFactory(acmeListing("api")), nil)
	require.NoError(t, err)
	failing := acmeListing()
	failing.Errors = map[string]error{"ListOrgRepos": assert.AnError}
	progress := &progressLog{}
	kept, dropped, err := reconcileGlobSelection(ctx, m, newTestClientFactory(failing), preflight, nil, selected, progress.fn)
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "listing glob entries' orgs again")
	assert.Nil(t, kept)
	assert.Nil(t, dropped)
	assert.Empty(t, progress.all())
}

// Converge fails before any probe or write when the live listing it
// compares GlobListing with fails.
func TestConverge_GlobListingLiveListingErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	live := acmeListing("api")
	preflight := preflightOrgListings(t, m, live)
	live.Errors = map[string]error{"ListOrgRepos": assert.AnError}
	progress := &progressLog{}
	result, err := Converge(ctx, ConvergeConfig{
		Manifest:       m,
		MaxConcurrency: 1,
		GlobListing:    preflight,
	}, newTestClientFactory(live), nil, progress.fn)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, assert.AnError)
	assert.Empty(t, progress.all())
	assertNoForgeWrites(t, live)
}

// A repository reconcileGlobSelection drops still counts as a consumer of
// the credential inputs, as an excluded one does: when it is the only
// consumer of a supplied credential flag, the run still converges the other
// repositories instead of rejecting the flag as unconsumed. A flag no
// selected repository uses, dropped or not, is still rejected.
func TestConverge_GlobListingDroppedRepoCountsAsCredentialConsumer(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		healthyAuth string
		dropped     RepoEntry
		set         func(*ConvergeConfig)
		// prepare gives the healthy repository the credentials its own
		// inference.auth needs, so only the dropped one consumes the flag.
		prepare func(live *forge.FakeClient)
		wantErr string
	}{
		"openai-api-key": {
			healthyAuth: InferenceAuthVertexWIF,
			dropped:     RepoEntry{Name: "acme/docs", Inference: InferenceSettings{Auth: InferenceAuthOpenAIAPIKey}},
			set:         func(c *ConvergeConfig) { c.OpenAIAPIKey = testOpenAIAPIKey },
		},
		"vertex": {
			healthyAuth: InferenceAuthOpenAIAPIKey,
			dropped:     RepoEntry{Name: "acme/docs", Inference: InferenceSettings{Auth: InferenceAuthVertexWIF}},
			set:         func(c *ConvergeConfig) { c.InferenceProject = "test-inference" },
			prepare: func(live *forge.FakeClient) {
				live.Secrets["acme/api/FULLSEND_OPENAI_API_KEY"] = true
			},
		},
		"jira": {
			healthyAuth: InferenceAuthVertexWIF,
			dropped:     RepoEntry{Name: "acme/docs"},
			set:         func(c *ConvergeConfig) { c.JiraToken = testJiraToken },
		},
		"unconsumed flag still rejected": {
			healthyAuth: InferenceAuthVertexWIF,
			dropped:     RepoEntry{Name: "acme/docs", Inference: InferenceSettings{Auth: InferenceAuthVertexWIF}},
			set:         func(c *ConvergeConfig) { c.OpenAIAPIKey = testOpenAIAPIKey },
			wantErr:     "--openai-api-key was supplied but no selected repository uses",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "jira" {
				tc.dropped.ConfigBase.Source = writePresetFile(t, jiraPollEnabledConfig)
			}
			m := newConvergeManifest()
			m.Defaults.Inference.Auth = tc.healthyAuth
			m.GitHub.Repos = []RepoEntry{{Name: "acme/*"}, tc.dropped}
			preflight := preflightOrgListings(t, m, newFakeClientForBatch("acme/api", "acme/docs"))
			live := newFakeClientForBatch("acme/api")
			markFullyInstalled(live, "acme", "api")
			if tc.prepare != nil {
				tc.prepare(live)
			}
			cfg := withoutInferenceInputs(convergeCfgWithDefaults(m))
			cfg.DryRun = true
			cfg.GlobListing = preflight
			tc.set(&cfg)
			progress := &progressLog{}

			result, err := Converge(ctx, cfg, newTestClientFactory(live), (&fakeScaffoldCommit{}).fn(), progress.fn)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Results, 1)
			assert.Equal(t, "api", result.Results[0].Repo)
			assert.NoError(t, result.Results[0].Error)
			assert.Contains(t, progress.all(), "acme/docs warning not installed: it is no longer listed in its org (deleted, archived or transferred) since this run's preflights evaluated the repository selection")
			assertNoForgeWrites(t, live)
		})
	}
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
