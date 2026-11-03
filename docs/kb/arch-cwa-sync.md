---
id: arch-cwa-sync
covers:
  - internal/accounts/**
  - internal/cwasync/**
see_also:
  - arch-matching
  - arch-kobo-traffic
---
# Accounts and the Hardcover to CWA sync

Hardcover is the source of truth for what has been read. The Kobo reports to
Hardcover through the worker. CWA records the Kobo's own reports natively, and this
sync corrects CWA to match Hardcover, so a book read elsewhere shows as finished on
the Kobo after its next sync. CWA never writes to Hardcover.

## INVARIANT

A user is the CWA sync token in the Kobo's request path. The store, logs and
events only ever see accounts.Key, a hash of it. The token itself lives in the
accounts file and in memory, and the CWA client's errors never carry a URL,
because the token is a path segment.

Books are keyed by user and entitlement id. CWA's books have the same id for
every user, so keying by id alone would merge two people's progress and matches.

A Kobo no account claims is saved but never matched or written, so a second
person's device can't write to the first person's Hardcover. The Any account,
with no Kobo token, takes every device. It only exists alone, for a single-user
setup whose token isn't configured yet, and it can't run this sync.

The sync only sets Finished when Hardcover has the book as read, and only undoes
a Finished it set itself. A book finished on the Kobo or in CWA's web page is
never unmarked, and a book the Kobo is partway through is left alone, because
that's a re-read. Progress is never copied to CWA, because Hardcover's is behind
the device's, and sending it back could move the reader's place.

Only resolved matches are synced. The sync matches CWA books the same way the
worker does, and its review decisions appear on the activity page.

The CWA sync state is its own table rather than fields on store.Book, because the
worker saves whole books across slow network calls and would overwrite it.

## GOTCHA

Read status in CWA is per user, and its Kobo sync reads only that user's status.
An admin can't set it for someone else: the web toggle and "mark selected" write
for whoever is logged in (cps/helper.py:356, v4.0.8). A Calibre read column can
be set by an admin, but the Kobo sync ignores it (cps/kobo.py:1071). So the sync
uses CWA's Kobo state endpoint with that user's token, as their device would.

CWA's state PUT handler needs a CurrentBookmark with ProgressPercent and
ContentSourceProgressPercent and a Statistics key, or it answers 400 or 500
(cps/kobo.py:948-1010). SetStatus sends back the bookmark it read.

A state GET creates an unread state for a book that had none, which the Kobo then
receives on its next sync. If the device had unsent progress for that book, the
newer server state might win, so the sync only reads books it may change: those
read on Hardcover, and those it marked itself. Dry run reads nothing.

A state request for a book CWA doesn't hold is redirected to Kobo's store. The
client doesn't follow redirects, so a token never leaves for Kobo.

The OPDS feed has no identifiers, so CWA books match on title and author only.

The library list uses the claude-robot CWA account over OPDS basic auth. It can't
read anyone's status, which is why the Kobo token is needed as well.

## DECISION

CWA's Kobo state endpoint rather than writing app.db, 2026-11-03. It runs CWA's
own code, which updates the timestamp the Kobo sync checks (cps/ub.py:735).
Writing app.db from the sidecar would depend on CWA's schema and skip that hook.

Accounts keyed by the CWA token in the path, 2026-11-03, because every Kobo
request already carries it. A setup page per user is deferred until a second user
exists, since it needs a login and keys stored at rest.

## VERIFY

    go test -race ./internal/accounts/ ./internal/cwasync/

Not yet seen live: the Kobo applying a Finished set this way on its next sync,
and whether it then reports the book Finished back. Reconcile adds no read to a
book already read on Hardcover, so an echo is harmless either way.

## SEE ALSO

- [arch-matching.md](arch-matching.md) - how the matches this relies on are made,
  and the writes the worker sends.
- [arch-kobo-traffic.md](arch-kobo-traffic.md) - what reaches CWA from the Kobo.
