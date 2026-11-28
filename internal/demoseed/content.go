package demoseed

// The seeded content: the guide is the demo repository's README (this file
// you are reading in the tree browser IS the tour), the source samples are
// small but real enough for the syntax highlighter and the blame view.

// Guide is the demonstration walkthrough (the demo repo's README, and the
// copy banyand drops beside the data as demo-guide.md).
const Guide = `# banyan — the guided playground

This repository was seeded by ` + "`demo: true`" + ` on a fresh deployment. Everything
here is real content on the real write path — clone it, push to it, break
it.

## Accounts (password: demo)

| Account | Role |
|---|---|
| ` + "`admin`" + ` | deployment administrator — sees every branch, merges anything |
| ` + "`alice`" + ` | owner of the ` + "`platform`" + ` group (the demo repository lives in it) |
| ` + "`bob`" + ` | viewer of the group — reads, opens merge requests, cannot push |

Change any password from **Settings → Profile** (the seeded one is not a
secret). SSH keys: **Settings → SSH Keys**, then clone
` + "`ssh://git@<host>/r/demo.git`" + `.

## The tour

1. **Subtrees** — ` + "`src/`" + ` and ` + "`docs/`" + ` each own a directory timeline
   (open the repository's **Subtrees** page). Their latest commits were
   pushed as subtree timelines: clone one directory alone with
   ` + "`git clone <url>/r/demo.git/tree/src`" + ` — one URL, one credential per
   subtree.
2. **Merge requests** — ` + "`!1`" + ` is open and mergeable (one approval on it;
   log in as alice and merge it), ` + "`!2`" + ` conflicts on purpose (open it,
   watch the live conflict banner, resolve it inline from the diff view),
   ` + "`!3`" + ` is already merged (the two-parent commit shows in the history).
3. **Branch protection** — ` + "`release`" + ` carries an merge-request-only rule: direct
   pushes bounce for everyone (try it), only a merge request with one
   approval gets in. See **Settings → Branch Protection**.
4. **Members** — the **Members** page lists alice (owner) and bob (viewer),
   both inherited from the ` + "`platform`" + ` group — the badge links there. A
   repository-level grant must rank strictly above the inherited one.
5. **Personal branches** — ` + "`alice/wip`" + ` exists but only alice and the
   admin can see it (the ref advertisement is filtered per identity — what
   your git client sees is what this console shows).

## Push something

` + "```bash" + `
git clone http://<host>/r/platform/demo.git
cd demo
echo "hello from the tour" >> docs/notes.md
git checkout -b tour
git commit -am "tour: leave a note"
git push origin tour
` + "```" + `

Branch names are single segments in banyan (the timeline model) — ` + "`tour`" + `,
not ` + "`feature/tour`" + `. Open the pushed branch as a merge request from the
console, or try pushing to ` + "`release`" + ` and watch the merge-request-only rule bounce it.

Have fun — the tree becomes a forest.
`

const helloGo = `package main

import "fmt"

// Greeter builds the greeting pipeline.
type Greeter struct {
	Name string
}

// Greet renders the greeting for one recipient.
func (g Greeter) Greet() string {
	return fmt.Sprintf("hello, %s", g.Name)
}

func main() {
	fmt.Println(Greeter{Name: "banyan"}.Greet())
}
`

const notesMd = `# working notes

- the playground is seeded, not sacred — push freely
- directory timelines live under src/ and docs/
`

// notesBranchSide and notesMainSide are the conflicting pair: the same
// opening line rewritten twice after the fork.
const notesBranchSide = `# working notes

the opening line was retitled on the branch.

- the playground is seeded, not sacred — push freely
- directory timelines live under src/ and docs/
`

const notesMainSide = `# working notes

the opening line was retitled on main instead.

- the playground is seeded, not sacred — push freely
- directory timelines live under src/ and docs/
`

const statsPy = `#!/usr/bin/env python3
"""Count the words of every note (the subtree's own little tool)."""
from pathlib import Path


def words(path: Path) -> int:
    return len(path.read_text(encoding="utf-8").split())


if __name__ == "__main__":
    for note in Path("docs").glob("*.md"):
        print(f"{note.name}: {words(note)} words")
`

const timelineMd = `# the timeline model

Every directory may own a commit history of its own. This one started as
an anchored push to ` + "`/r/demo.git/tree/docs`" + ` — from the outside it is a plain
git remote; on the inside the root view weaves it into the repository you
browse.
`

const greetTs = `export function greet(name: string): string {
  return ` + "`hello, ${name}`" + `;
}

console.log(greet("banyan"));
`

const changelogMd = `# changelog

- seeded: the playground, two subtree timelines, three merge requests
`
