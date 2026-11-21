package meta

// The repository description roundtrip (docs/design/data-model.md §15's
// display metadata): set, read back through GetRepo and ListRepos, clear;
// a missing repo answers ErrRepoNotFound on every backend.
import (
	"context"
	"errors"
	"testing"
)

func TestRepoDescription(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			ctx := context.Background()
			if err := ms.CreateRepo(ctx, "r", 0); err != nil {
				t.Fatal(err)
			}
			if err := ms.SetRepoDescription(ctx, "r", "the playground repository"); err != nil {
				t.Fatalf("set: %v", err)
			}
			got, err := ms.GetRepo(ctx, "r")
			if err != nil || got.Description != "the playground repository" {
				t.Fatalf("get: %+v err=%v", got, err)
			}
			list, err := ms.ListRepos(ctx)
			if err != nil || len(list) != 1 || list[0].Description != "the playground repository" {
				t.Fatalf("list: %+v err=%v", list, err)
			}
			if err := ms.SetRepoDescription(ctx, "r", ""); err != nil {
				t.Fatalf("clear: %v", err)
			}
			if got, _ := ms.GetRepo(ctx, "r"); got.Description != "" {
				t.Fatalf("after clear: %+v", got)
			}
			if err := ms.SetRepoDescription(ctx, "nope", "x"); !errors.Is(err, ErrRepoNotFound) {
				t.Fatalf("missing repo: %v", err)
			}
		})
	}
}
