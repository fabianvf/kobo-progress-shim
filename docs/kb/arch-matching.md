---
id: arch-matching
covers:
  - cmd/match-eval/**
  - internal/activity/**
  - internal/dryrun/**
  - internal/hardcover/**
  - internal/kobostore/**
  - internal/match/**
  - internal/store/**
see_also:
  - arch-capture
  - arch-kobo-traffic
---
# Matching Kobo books to Hardcover

How a book reported by the Kobo becomes a Hardcover book id. A wrong match
publishes false reading history on a public profile, and bad matches are what
ruined the last setup of this kind, so uncertainty goes to review.

## INVARIANT

An automatic match needs two signals that agree: an ISBN that finds one book whose
title and author also match, or exactly one exact title and author match from
search. A stored hardcover-id is the only single signal that counts, because a
person put it there. An ISBN with no title to check goes to review.

Search order is never trusted. Junk listings can rank above the real book.

KoboReader.sqlite is never an input. It's the answer key for cmd/match-eval, and
a system that read it would need USB access that the live path doesn't have.

Kobo auth stays in memory for one store call. capture.Progress holds it, so never
print that struct whole or save it. store.Book has no auth field.

A match chosen by hand outlives later syncs. Only an unresolved book takes new
metadata from sync, otherwise a sync would undo a correction.

A store that won't open disables matching and leaves the proxy running, because
the Kobo's path must not depend on it.

Hardcover writes reconcile against its current state rather than replaying events,
which is what stops repeated syncs creating duplicate reads. Read the book back after
inserting it, because adding a book may create a read row. Whether it does isn't
known yet, and the code handles both.

Only DRY_RUN=false writes. Anything else stays dry, because a live write publishes
to a public profile. Switching to live doesn't replay earlier dry-run progress: only
a book's next report or a hand-chosen match sends anything.

The activity page has no login, so anyone on the LAN can save a match, which then
drives public writes. It refuses cross-site posts, and must go behind Cloudflare
Access if it's ever exposed remotely.

## GOTCHA

Hardcover's read input takes progress_pages or progress_seconds, not a percent, so
progress needs an edition with a page count. The edition comes from the ISBN, then
from the book already on the shelf.

Read dates are UTC, so a late evening's read can land on the next day.

The od_ prefix in ExternalIds doesn't mean a book came from Libby. Store books
have them too, and some Libby books don't.

The Kobo's Isbn appears in only some bookmark paths (17 of 74 books in the answer
key, all correct), so a bookmark ISBN supports a match but can't make one.

Bracketed notes in Kobo titles such as "(with bonus short story)" pull unrelated
books into search results. Strip them before searching.

Hardcover has duplicate books for the same title and author, which is the main
reason title and author alone go to review. Hardcover can also merge books, so a
saved id may move (merged_at, original_book_id on user_books).

CWA's books aren't in the store, so a lookup for one should return not found. Their
title and author come from sync entries. A book already on the device before the
store existed has no entry until CWA next sends one. Reading metadata.db for a
hardcover-id isn't built.

A rating request uses the store's RevisionId, not the EntitlementId, and CWA answers
404 for its own books. Only a CWA book's rating has been seen. Ratings are the first
thing to drop if that link proves unreliable.

The store lookup returned 403 until it forwarded the device's headers, as CWA does.
Which header matters isn't known, so don't trim the list without testing.

## DECISION

Identify from live signals and measure against the Kobo's database, rather than
importing a mapping from it, 2026-11-02. An import only covers books already on the
device, and it hides whether the live path works.

Store lookup at /v1/library/<id>/metadata, 2026-11-02. It returned ISBN, title and
author for three Libby loans, using the device's AuthToken from a one-off script.

## VERIFY

    go test -race ./...
    HARDCOVER_API_KEY=... go run ./cmd/match-eval -key <answer key csv>

The last run on 77 books: 72 automatic and 5 review with ISBN, title and author; 64
automatic and 13 review with title and author, with no conflicts between them. Those
answers aren't checked by hand and the first row has no independent check.

Live on v0.2.1: a Libby and a store book each returned ISBN, title and author, and
both matched automatically when run through the matcher separately. Not yet seen:
a CWA book's not-found response, or a new book arriving in sync. The write client has
only run against a fake Hardcover, built from the real schema by introspection.

## SEE ALSO

- [arch-kobo-traffic.md](arch-kobo-traffic.md) - which requests carry progress and
  what each one exposes.
- [arch-capture.md](arch-capture.md) - the proxy invariants this must not break.
