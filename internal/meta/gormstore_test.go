package meta

// The row models are pinned to the live schema (docs/storage/storage.md §2.3
// discipline): GORM's naming strategy is implicit convention — the registered
// trap is OID snake-casing to "o_id" — and AssertModelColumns turns a
// mismatch into an open-time test failure instead of a query-time surprise.
import (
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/ddl"
)

func TestRowModelsMatchSchema(t *testing.T) {
	s, err := NewSQLiteMetaStore(filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := ddl.AssertModelColumns(s.orm,
		repoRow{}, timelineRow{}, packRow{}, packNodeRow{}, objectRow{}, placementRow{},
		hookRow{}, repoBlobRow{}, repoPoolRow{}, renameFenceRow{}, tokenRow{}, subtreeRuleRow{},
		mrRow{}, mrApprovalRow{}, branchRuleRow{}, groupRow{}, repoGroupRow{},
	); err != nil {
		t.Fatal(err)
	}
}
