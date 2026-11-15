package console

// The console's row models pinned to its schema — the same naming-strategy
// guard as the metadata plane (docs/storage/storage.md §2.3).
import (
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/ddl"
)

func TestRowModelsMatchSchema(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := ddl.AssertModelColumns(d.orm,
		userRow{}, tokenRow{}, nodeRow{}, memberRow{},
	); err != nil {
		t.Fatal(err)
	}
}
