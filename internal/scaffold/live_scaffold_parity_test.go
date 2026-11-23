package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
)

// liveDispatchUsesRe matches the reusable-dispatch `uses:` line of a
// per-repo shim and captures the pinned ref and the optional version tag
// from its traceability comment.
var liveDispatchUsesRe = regexp.MustCompile(
	`(?m)^\s*uses:\s*` + regexp.QuoteMeta(config.DefaultUpstreamRepo) +
		`/\.github/workflows/reusable-dispatch\.yml@(\S+)(?:[ \t]+#[ \t]*(\S+))?[ \t]*$`)

// liveUpstreamPin returns the upstream ref and version tag that the given
// per-repo shim pins its reusable-dispatch call to.
func liveUpstreamPin(shim string) (ref, tag string, err error) {
	m := liveDispatchUsesRe.FindStringSubmatch(shim)
	if m == nil {
		return "", "", fmt.Errorf("no %s reusable-dispatch.yml uses: line found", config.DefaultUpstreamRepo)
	}
	return m[1], m[2], nil
}

func TestLiveUpstreamPin(t *testing.T) {
	const prefix = "jobs:\n  dispatch:\n    uses: fullsend-ai/fullsend/.github/workflows/reusable-dispatch.yml@"
	tests := []struct {
		name    string
		shim    string
		wantRef string
		wantTag string
		wantErr bool
	}{
		{name: "sha with tag", shim: prefix + "d11de3ea0dde5aa652110b6716c19e000c426df7  # v0.57.1\n", wantRef: "d11de3ea0dde5aa652110b6716c19e000c426df7", wantTag: "v0.57.1"},
		{name: "bare sha", shim: prefix + "d11de3ea0dde5aa652110b6716c19e000c426df7\n", wantRef: "d11de3ea0dde5aa652110b6716c19e000c426df7"},
		{name: "tag ref", shim: prefix + "v0.57.1\n", wantRef: "v0.57.1"},
		{name: "main", shim: prefix + "main\n", wantRef: "main"},
		{name: "vendored", shim: "jobs:\n  dispatch:\n    uses: ./.github/workflows/reusable-dispatch.yml\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, tag, err := liveUpstreamPin(tt.shim)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRef, ref)
			assert.Equal(t, tt.wantTag, tag)
		})
	}
}

// TestLivePerRepoScaffoldFilesMatchInstallRender checks that this
// repository's own per-repo shim and thin callers are byte-for-byte what
// `fullsend repos install` and `fullsend github setup` render for a
// repository installed with the default runner. The workflow_call
// alignment tests compare inputs and secrets only, so without this check a
// fix that lands in a template does not reach the live workflow, and a hand
// edit to the live workflow is lost on the next converge (#8513).
//
// The upstream ref and version tag are read from the live shim's
// reusable-dispatch `uses:` line rather than assumed to be the default
// ref, so a repository pinned to a release (e.g. `@<sha>  # v0.57.1`)
// still gets a strict comparison of every file, pin format included.
func TestLivePerRepoScaffoldFilesMatchInstallRender(t *testing.T) {
	shim, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "fullsend.yaml"))
	require.NoError(t, err)
	ref, tag, err := liveUpstreamPin(string(shim))
	require.NoError(t, err, "reading the upstream pin from the live shim")

	files, err := CollectPerRepoInstallFiles(false, ref, tag)
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		t.Run(f.Path, func(t *testing.T) {
			live, err := os.ReadFile(filepath.Join("..", "..", f.Path))
			require.NoError(t, err, "live scaffold file %s", f.Path)
			assert.Equal(t, string(f.Content), string(live),
				"%s differs from the per-repo install render of its template; change the template (and re-render the live file) or revert the live edit so both agree", f.Path)
		})
	}
}
