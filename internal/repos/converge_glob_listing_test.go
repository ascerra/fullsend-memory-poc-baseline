package repos

import (
	"context"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type progressLine struct{ repo, phase, msg string }

func recordProgress(lines *[]progressLine) ProgressFunc {
	return func(repo, phase, msg string) { *lines = append(*lines, progressLine{repo, phase, msg}) }
}

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

// Repositories the live listing shows but the selection lacks are reported,
// unless the repository filter does not select them.
func TestReportReposListedAfterSelection(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	selected, err := m.ExpandGlobsFor(ctx, newTestClientFactory(acmeListing("api")), nil)
	require.NoError(t, err)
	live := newTestClientFactory(acmeListing("api", "web", "docs"))

	t.Run("no filter", func(t *testing.T) {
		var lines []progressLine
		reportReposListedAfterSelection(ctx, m, live, nil, selected, recordProgress(&lines))
		require.Len(t, lines, 2)
		assert.Equal(t, "acme/docs", lines[0].repo)
		assert.Equal(t, "acme/web", lines[1].repo)
		for _, l := range lines {
			assert.Equal(t, "warning", l.phase)
			assert.Contains(t, l.msg, "run repos install again")
		}
	})
	t.Run("filter", func(t *testing.T) {
		var lines []progressLine
		reportReposListedAfterSelection(ctx, m, live, []string{"acme/api", "ACME/w*"}, selected, recordProgress(&lines))
		require.Len(t, lines, 1)
		assert.Equal(t, "acme/web", lines[0].repo)
	})
	t.Run("nothing new", func(t *testing.T) {
		var lines []progressLine
		reportReposListedAfterSelection(ctx, m, newTestClientFactory(acmeListing("api")), nil, selected, recordProgress(&lines))
		assert.Empty(t, lines)
	})
}

// A live listing that fails is reported and does not stop convergence.
func TestReportReposListedAfterSelection_ListingError(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	failing := acmeListing("api", "web")
	failing.Errors = map[string]error{"ListOrgRepos": assert.AnError}
	var lines []progressLine
	reportReposListedAfterSelection(ctx, m, newTestClientFactory(failing), nil, nil, recordProgress(&lines))
	require.Len(t, lines, 1)
	assert.Equal(t, "", lines[0].repo)
	assert.Equal(t, "warning", lines[0].phase)
	assert.Contains(t, lines[0].msg, assert.AnError.Error())
}

func TestMatchesAnyPattern(t *testing.T) {
	assert.True(t, matchesAnyPattern(nil, "acme/api"), "an empty filter matches everything")
	assert.True(t, matchesAnyPattern([]string{"other/x", "acme/*"}, "acme/api"))
	assert.False(t, matchesAnyPattern([]string{"other/*"}, "acme/api"))
	assert.False(t, matchesAnyPattern([]string{"acme/["}, "acme/api"), "an invalid pattern matches nothing")
}

// Converge expands glob entries through GlobListing, so a repository only
// the live clients list is neither probed nor written, and is reported.
func TestConverge_GlobListingSelectsRepos(t *testing.T) {
	ctx := context.Background()
	m := globListingManifest(t)
	live := acmeListing("web")
	var lines []progressLine
	result, err := Converge(ctx, ConvergeConfig{
		Manifest:       m,
		DryRun:         true,
		MaxConcurrency: 1,
		GlobListing:    newTestClientFactory(acmeListing()),
	}, newTestClientFactory(live), nil, recordProgress(&lines))
	require.NoError(t, err)
	assert.Empty(t, result.Results, "the selection the preflights saw is empty")
	require.Len(t, lines, 1)
	assert.Equal(t, "acme/web", lines[0].repo)
	assert.Contains(t, lines[0].msg, "not installed")
}
