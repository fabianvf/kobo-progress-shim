---
id: arch-matching
covers:
  - cmd/match-eval/**
  - internal/dryrun/**
  - internal/hardcover/**
  - internal/kobostore/**
  - internal/match/**
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
print that struct whole. dryrun.Entry has no auth field and is what gets logged.

## GOTCHA

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
signals have to come from sync entries or metadata.db, which isn't built yet.

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

Not yet seen: a store book through the lookup, and a successful lookup using auth
from a live PUT. v0.2.0 sent only Authorization, User-Agent and Accept from the live
PUT and got 403 on two books. A one-off script using the device's stored token with
those same headers got 200, so the sidecar now forwards the device's headers the way
CWA does, apart from hop, session and body headers. That's a likely fix, not a
confirmed one.

## SEE ALSO

- [arch-kobo-traffic.md](arch-kobo-traffic.md) - which requests carry progress and
  what each one exposes.
- [arch-capture.md](arch-capture.md) - the proxy invariants this must not break.
