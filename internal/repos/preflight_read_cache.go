package repos

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// NewPreflightReadCache wraps clients so that repeated repository-state
// reads return the first answer instead of calling the forge again: file
// contents on the default branch, repository variables, secret existence
// and org repository listings (#8501). Every other forge.Client method is
// passed through unchanged.
//
// The cache never observes a write it did not make, so it is only correct
// for a window in which nothing writes to the forge. `repos install` uses
// one for its read-only configuration preflights (ownership, install
// defaults, trace signing keys, proposed safety, effective infrastructure)
// and does not read through it once the writes begin. Convergence keeps
// reading current state through the original clients, and takes from the
// cache only a frozen snapshot of its org listings (OrgListings) to expand
// glob entries. Successful reads and not-found answers are cached; any other
// error is returned uncached.
func NewPreflightReadCache(clients ForgeClientFactory) *PreflightReadCache {
	return &PreflightReadCache{clients: clients, byForge: make(map[string]*cachingReadClient)}
}

// PreflightReadCache is the ForgeClientFactory NewPreflightReadCache returns.
type PreflightReadCache struct {
	clients ForgeClientFactory

	mu      sync.Mutex
	byForge map[string]*cachingReadClient
}

// ConfigFor returns the wrapped factory's ForgeConfig with its client
// replaced by a caching client shared by every call for the same forge
// (the wrapped factory caches its clients per forge too). Factory errors
// are returned as-is and not cached.
func (c *PreflightReadCache) ConfigFor(forgeName string) (ForgeConfig, error) {
	fc, err := c.clients.ConfigFor(forgeName)
	if err != nil {
		return fc, err
	}
	if fc.Client == nil {
		return fc, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.byForge[forgeName]
	if !ok {
		cached = newCachingReadClient(fc.Client)
		c.byForge[forgeName] = cached
	}
	fc.Client = cached
	return fc, nil
}

type fileReadResult struct {
	data []byte
	err  error
}

type variableReadResult struct {
	value  string
	exists bool
}

type orgReposKey struct {
	org            string
	includePrivate bool
}

// cachingReadClient memoizes the repository-state reads of the embedded
// client. See NewPreflightReadCache for when that is safe.
type cachingReadClient struct {
	forge.Client

	mu        sync.Mutex
	files     map[[3]string]fileReadResult
	variables map[[3]string]variableReadResult
	secrets   map[[3]string]bool
	orgRepos  map[orgReposKey][]forge.Repository
}

func newCachingReadClient(client forge.Client) *cachingReadClient {
	return &cachingReadClient{
		Client:    client,
		files:     make(map[[3]string]fileReadResult),
		variables: make(map[[3]string]variableReadResult),
		secrets:   make(map[[3]string]bool),
		orgRepos:  make(map[orgReposKey][]forge.Repository),
	}
}

// GetFileContent caches content and not-found answers per owner, repo and
// path. Each caller receives its own copy of the content.
func (c *cachingReadClient) GetFileContent(ctx context.Context, owner, repo, path string) ([]byte, error) {
	key := [3]string{owner, repo, path}
	c.mu.Lock()
	r, ok := c.files[key]
	c.mu.Unlock()
	if ok {
		return slices.Clone(r.data), r.err
	}
	data, err := c.Client.GetFileContent(ctx, owner, repo, path)
	if err != nil && !forge.IsNotFound(err) {
		return data, err
	}
	c.mu.Lock()
	c.files[key] = fileReadResult{data: slices.Clone(data), err: err}
	c.mu.Unlock()
	return data, err
}

// GetRepoVariable caches the value and existence per owner, repo and name.
func (c *cachingReadClient) GetRepoVariable(ctx context.Context, owner, repo, name string) (string, bool, error) {
	key := [3]string{owner, repo, name}
	c.mu.Lock()
	r, ok := c.variables[key]
	c.mu.Unlock()
	if ok {
		return r.value, r.exists, nil
	}
	value, exists, err := c.Client.GetRepoVariable(ctx, owner, repo, name)
	if err != nil {
		return value, exists, err
	}
	c.mu.Lock()
	c.variables[key] = variableReadResult{value: value, exists: exists}
	c.mu.Unlock()
	return value, exists, nil
}

// RepoSecretExists caches existence per owner, repo and name.
func (c *cachingReadClient) RepoSecretExists(ctx context.Context, owner, repo, name string) (bool, error) {
	key := [3]string{owner, repo, name}
	c.mu.Lock()
	exists, ok := c.secrets[key]
	c.mu.Unlock()
	if ok {
		return exists, nil
	}
	exists, err := c.Client.RepoSecretExists(ctx, owner, repo, name)
	if err != nil {
		return exists, err
	}
	c.mu.Lock()
	c.secrets[key] = exists
	c.mu.Unlock()
	return exists, nil
}

// ListOrgRepos caches the listing per org and visibility, so each preflight
// expanding the same glob reuses one listing. The org is keyed as
// OrgListings keys it (orgListingKey), so globs whose orgs differ only in
// case, such as Acme/a* and acme/b*, share one listing and the snapshot
// never has to choose between two read at different moments. Each caller
// receives its own copy of the slice.
func (c *cachingReadClient) ListOrgRepos(ctx context.Context, org string, includePrivate bool) ([]forge.Repository, error) {
	key := orgListingKey(org, includePrivate)
	c.mu.Lock()
	r, ok := c.orgRepos[key]
	c.mu.Unlock()
	if ok {
		return slices.Clone(r), nil
	}
	repos, err := c.Client.ListOrgRepos(ctx, org, includePrivate)
	if err != nil {
		return repos, err
	}
	c.mu.Lock()
	c.orgRepos[key] = slices.Clone(repos)
	c.mu.Unlock()
	return repos, nil
}

// OrgListings returns a snapshot of the org repository listings the cache has
// read so far. `repos install` takes it once its preflights have run and
// passes it to convergence (ConvergeConfig.GlobListing), so convergence
// expands glob entries from exactly the listings the preflights evaluated.
// Later reads through the cache do not change the snapshot.
func (c *PreflightReadCache) OrgListings() *OrgListings {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := &OrgListings{byForge: make(map[string]map[orgReposKey][]forge.Repository, len(c.byForge))}
	for forgeName, client := range c.byForge {
		client.mu.Lock()
		listings := make(map[orgReposKey][]forge.Repository, len(client.orgRepos))
		for key, repos := range client.orgRepos {
			listings[key] = slices.Clone(repos)
		}
		client.mu.Unlock()
		l.byForge[forgeName] = listings
	}
	return l
}

// OrgListings is a fixed set of org repository listings per forge, org and
// visibility, taken by PreflightReadCache.OrgListings. It never lists an org
// itself: expanding a glob whose org it does not hold is an error rather
// than a live read, so convergence cannot select a repository the preflights
// did not evaluate. Org names are matched case-insensitively, as forges
// treat them and as repository names are compared.
type OrgListings struct {
	byForge map[string]map[orgReposKey][]forge.Repository
}

// orgListingKey returns the OrgListings key for org and visibility: the org
// name is lower-cased so that, for example, an explicit Acme/docs entry
// finds the listing an acme/* glob read.
func orgListingKey(org string, includePrivate bool) orgReposKey {
	return orgReposKey{org: strings.ToLower(org), includePrivate: includePrivate}
}

// lists reports whether l holds the listing glob expansion reads for rr's org
// (private repositories included) and, if it does, whether that listing
// contains rr.
func (l *OrgListings) lists(rr ResolvedRepo) (orgListed, repoListed bool) {
	repos, ok := l.byForge[rr.Forge][orgListingKey(rr.Owner, true)]
	if !ok {
		return false, false
	}
	for _, r := range repos {
		if strings.EqualFold(r.Name, rr.Repo) {
			return true, true
		}
	}
	return true, false
}

// clients returns a factory for expanding glob entries from l: its configs
// are live's, with clients whose ListOrgRepos answers only from l. Glob
// expansion reads nothing else.
func (l *OrgListings) clients(live ForgeClientFactory) ForgeClientFactory {
	return orgListingsFactory{listings: l, live: live}
}

type orgListingsFactory struct {
	listings *OrgListings
	live     ForgeClientFactory
}

func (f orgListingsFactory) ConfigFor(forgeName string) (ForgeConfig, error) {
	fc, err := f.live.ConfigFor(forgeName)
	if err != nil {
		return fc, err
	}
	fc.Client = &orgListingsClient{Client: fc.Client, forgeName: forgeName, listings: f.listings}
	return fc, nil
}

type orgListingsClient struct {
	forge.Client

	forgeName string
	listings  *OrgListings
}

// ListOrgRepos returns a copy of the snapshot's listing, or an error when
// the snapshot does not hold one.
func (c *orgListingsClient) ListOrgRepos(_ context.Context, org string, includePrivate bool) ([]forge.Repository, error) {
	repos, ok := c.listings.byForge[c.forgeName][orgListingKey(org, includePrivate)]
	if !ok {
		return nil, fmt.Errorf("org %q on %s was not listed by this run's preflights", org, c.forgeName)
	}
	return slices.Clone(repos), nil
}
