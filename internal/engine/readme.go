package engine

import (
	"os"
	"path/filepath"
)

// ensureReadme writes README.md into the tasks directory the first time
// anything is written there, and never touches it again — a person may
// reasonably edit or delete it, and a silent rewrite would erase that.
func ensureReadme(dir string) {
	path := filepath.Join(dir, "README.md")
	if _, err := os.Stat(path); err == nil {
		return
	}
	writeAtomic(path, []byte(readmeText), 0o644)
}

// readmeText is written to <project>/.be-code/tasks/README.md the first
// time the engine writes a document there (ensureReadme, called from
// Flush). It is the only place the task-document format is explained to a
// human, so it has to be accurate on its own — nobody reading it has this
// package's source open beside it. It documents the same format the model
// reads and writes through the task tool, because both edit these files.
const readmeText = `# Task records

BE-Code keeps its working memory here, in ` + "`.be-code/tasks/`" + ` inside
your project: one Markdown document per top-level task, named
` + "`NNN-<slug>.md`" + `. These are ordinary files, not a database. Open one
in an editor. Read it. Edit it by hand whenever you like — the next section
says what is safe to change.

## Untracked by default

This ` + "`.be-code/`" + ` folder lives in your project, next to your code —
it is not the same folder as ` + "`~/.be-code/`" + ` in your home directory,
which is where BE-Code keeps its own session and configuration state and
has nothing to do with this project. The one here holds only
` + "`tasks/`" + ` — this record.

If your project is a git repository, the first document written here adds
` + "`.be-code/`" + ` to your ` + "`.gitignore`" + `, so this folder never
shows up in ` + "`git status`" + ` uninvited. If you decide the task record
is worth keeping in your project's history, delete that one line —
` + "`.be-code/`" + ` — from ` + "`.gitignore`" + ` and commit
` + "`.be-code/tasks/`" + `. Outside a git repository nothing creates a
` + "`.gitignore`" + ` for you, so this folder is not hidden from
anything — unless a ` + "`.gitignore`" + ` is already there, in which case
the same line is still added to it, git repository or not.

## The format

A document is one heading and a nested checklist, with evidence tucked
under the step that earned it:

    # 001 — fix the parser

    - [x] 1. fix the parser
      - [x] 1.1. find the bug
        - files: lexer.go (lines 1–120)
        - cmds: go test ./... — failed
        - error: FAIL: TestLex
      - [>] 1.2. fix and verify
      - [-] 1.3. rewrite the scanner — dropped: not needed after all

Ids are dotted paths that follow a step's position in the tree, not
anything chosen by hand: ` + "`2.1.3`" + ` is the third child of the first
child of the second task. Indentation is two spaces per level. Evidence
lines sit one level deeper than the step they describe, each starting with
one of the keys ` + "`files:`" + `, ` + "`cmds:`" + `, ` + "`lookups:`" + `,
` + "`note:`" + `, ` + "`decision:`" + ` or ` + "`error:`" + `.

Five status marks, one per step:

  ` + "`[ ]`" + ` todo      not started
  ` + "`[>]`" + ` doing     in progress
  ` + "`[x]`" + ` done      finished
  ` + "`[!]`" + ` blocked   could not proceed
  ` + "`[-]`" + ` dropped   decided not to

A blocked or dropped step carries why, after an em dash — the ` +
	"`[-] 1.3. rewrite the scanner`" + ` line above is the shape.

## Editing these files by hand

Safe to change: a step's text, its status mark, its reason (the words after
the em dash on a blocked or dropped step), and any ` + "`note:`" + ` or
` + "`decision:`" + ` line — your own prose, or a correction to the
engine's.

Anything the engine does not recognise as a step or an evidence line —
prose, a heading of your own, an ordinary checklist you happen to keep in
the same file — is preserved exactly as written and comes back untouched on
the next save. A top-level checklist item is only ever read as a task when
it carries a number, so ` + "`- [ ] buy milk`" + ` at the bottom of the file
stays your own text; write ` + "`- [ ] 2. buy milk`" + ` if you want it
tracked as one.

Short of quarantining one it cannot parse (below), the engine never renames
or deletes a document once it exists, even when the task it describes is
retitled — a document keeps its file for life, so its file name can drift
out of step with the task's current title. The
` + "`# NNN — title`" + ` heading at the top is regenerated on every save
and is the one to trust; the file name is only ever a label.

## What the engine repairs for you, silently

A step's id is derived from where it sits in the tree, not from what you
typed. If you insert, delete or reorder steps by hand and the ids in the
file no longer match their positions, the engine renumbers them back into
position the next time it loads the document and leaves itself a note on
the affected step — ` + "`- note: id repaired from 1.3 to 1.2`" + ` — so
you can see it disagreed with what was on disk. This happens quietly,
without a warning printed anywhere; the note in the file is the whole
record of it.

## What it quarantines

A document the engine cannot parse at all — broken indentation, an unknown
status mark, a step nested under nothing — is never half-read or rewritten
over your work. It is renamed
` + "`NNN-<slug>.broken-<stamp>.md`" + ` in this same folder, its content
untouched, and a line is printed to the terminal saying so. The engine
treats that task as gone from its own memory until you fix the copy and
rename it back; nothing here is ever deleted.

## The one rule that matters to the model

Exactly one step across the whole record should be marked ` + "`[>]`" + `
doing at a time — it is how the model (and the harness's own summary of
where things stand) knows what is in progress. The engine keeps this true
on its own whenever it moves a step to doing.

It does not, however, choose for you: a hand-edited status is your own
intent, so the engine never overrides one, even a second ` + "`[>]`" + `
where only one belongs. If you end up with more than one, it simply uses
whichever it reads first and ignores the rest. Unlike a repaired id or a
quarantined document, there is no note to leave on either step for this —
nothing is wrong with either one alone, only with having two — so instead
the engine prints a line to the terminal every time it loads the record
while more than one is marked doing, naming the documents involved. Nothing
about your files changes; fixing it is one status mark away.

## The long form

Everything above is what you need to read and edit these files. The full
specification of the format — every evidence key, how a Task Report is
rolled up when a task finishes, and what the engine keeps in its own dotdir
rather than here — is ` + "`docs/task-format.md`" + ` in the BE-Code source
tree.
`
