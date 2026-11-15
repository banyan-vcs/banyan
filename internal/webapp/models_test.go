package webapp

// The web tier's row models pinned to its schema — the same naming-strategy
// guard as the metadata plane (docs/storage/storage.md §2.3).
import (
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/ddl"
)

func TestRowModelsMatchSchema(t *testing.T) {
	w, err := OpenDB("sqlite:" + filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := ddl.AssertModelColumns(w.orm,
		webSessionRow{}, webOAuthStateRow{}, webImportRow{}, webCLIGrantRow{},
	); err != nil {
		t.Fatal(err)
	}
}
