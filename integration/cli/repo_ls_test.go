package cli

// The repo ls tree: the registry's hierarchy link (docs/design/data-model.md §14)
// renders as indentation, multi-level, name-ordered inside each parent.
import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/integration/support"
)

func TestCLIRepoLsTree(t *testing.T) {
	s, ms, _ := support.NewServerPlane(t)
	for _, name := range []string{"monorepo", "svc-auth", "svc-auth-lib", "solo"} {
		require.NoError(t, ms.CreateRepo(t.Context(), name, model.SHA1))
	}
	require.NoError(t, ms.SetRepoParent(t.Context(), "svc-auth", "monorepo"))
	require.NoError(t, ms.SetRepoParent(t.Context(), "svc-auth-lib", "svc-auth"))

	t.Setenv("BANYAN_SERVER", s.URL)
	out, errOut, code := runCLI(t, "repo", "ls")
	require.Equal(t, 0, code, "stderr:%s", errOut)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 4, "one line per repo:\n%s", out)
	assert.Equal(t, "monorepo\tmain (sha1)", lines[0])
	assert.Equal(t, "  svc-auth\tmain (sha1)", lines[1], "children indent under the parent")
	assert.Equal(t, "    svc-auth-lib\tmain (sha1)", lines[2], "multi-level nesting compounds the indent")
	assert.Equal(t, "solo\tmain (sha1)", lines[3], "roots resume at column zero")
}
