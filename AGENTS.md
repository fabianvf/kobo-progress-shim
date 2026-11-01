## Knowledge base

Implementation knowledge, decision rationale and cross-file invariants live in
`docs/kb/` - see `docs/kb/INDEX.md`.

**Read the relevant article BEFORE reading source code or making changes.** It
holds context that is not in the code: which files must change together, which
plausible edit has already broken production, and why the current shape was
chosen.

To find what to read, look the file up rather than grepping:

    kb owners path/to/file.go

Before committing, run `kb fresh`. If it names articles, re-read every one of
them against the changed files, then run the exact command `kb fresh` told you
to run. Never record an article you did not open.
