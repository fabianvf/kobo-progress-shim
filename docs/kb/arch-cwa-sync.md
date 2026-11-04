---
id: arch-cwa-sync
covers:
  - internal/accounts/**
  - internal/cwasync/**
  - internal/setup/**
  - internal/wanted/**
see_also:
  - arch-matching
  - arch-kobo-traffic
---
# Accounts, setup and the Hardcover to CWA sync

Hardcover is the source of truth. A reader sets up their Kobo and Hardcover and
never deals with CWA: the shim makes their CWA account, their Kobo collections
follow their Hardcover shelves and lists, Want to Read is downloaded, and read
status follows Hardcover. The Kobo reports to Hardcover through the worker. CWA
never writes to Hardcover.

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
worker does, and its review decisions appear on the activity page. A book that
doesn't match is also checked against the shim's own Want to Read requests: exactly
one request with an agreeing author and a fitting title, after dropping a series
marker like "SS 04:", decides. Imports arrive with release-style titles, and only
requested books qualify, which is what makes the looser title safe.

The CWA sync state is its own table rather than fields on store.Book, because the
worker saves whole books across slow network calls and would overwrite it.

Collections only change what the sync added. A book added on the Kobo stays, and
one the sync added and the Kobo removed comes back only if it changes on Hardcover.
"Hardcover" holds every book on any shelf or list, which for a shelf-only account
is everything on the device. A collection deleted on the Kobo is made again: each
run renames every collection to its own name, the only way to ask CWA whether a
shelf still exists, and a gone one is recreated.

Progress goes to CWA only for a copy the Kobo has never opened, with no bookmark
location. After that the Kobo owns its place in the book.

Want to Read downloads one book at a time, never an author, and never queues a
book LazyLibrarian already has Open, Have, Wanted, Snatched or Processed. Only
books the library doesn't hold, by the shim's own CWA matches, are considered, so
what LazyLibrarian remembers can't cause a re-download, and its database can be
wiped. A book asked for before is asked again only if LazyLibrarian no longer holds
it. With LL_SOURCE=HardCover the Hardcover id is tried first, since LazyLibrarian's
main source is then Hardcover too; OpenLibrary is the fallback, and its work must
agree on title and author. One book's failure is logged and the rest go on.

Setup is served at /setup/ on the library's public address. The tunnel sends that
path to the setup port, so it's behind the same Access app, and the /kobo bypass
doesn't cover it. Setup trusts only Cloudflare Access's signed token, checked against the team's
keys with the application's AUD, issuer and expiry. On the LAN the setup port can
be reached without passing Access, so a bare email header proves nothing. Each
email gets one account and can only see or change its own. A configured account
with an email (ACCOUNT_EMAIL, or email in ACCOUNTS_FILE) is shown on the page but
not changed there, and setup won't make that email a second account. Accounts made there
are sealed with ACCOUNTS_KEY, which the store never holds, and can't claim a Kobo
a configured account already has. The page shows the signed-in reader their own
CWA login, for browsing on the web. The CWA username is the reader's Hardcover
username, with a number added if CWA already has that name. Their CWA user has no password-change role,
because the shim needs that password to mark their shelves to sync.

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
client doesn't follow redirects, so a token never leaves for Kobo. A collection
request for a tag CWA doesn't know is proxied to Kobo's store by CWA itself, which
answered 403, and on 2026-11-04, after a store outage, 401. So 401, 403 and 404
all mean the collection is gone. A bad token also gets a 401 from CWA, but the
sync's state calls use the token before the collections do, and would fail first.
The first run after Fabian deleted his collections stopped on that 401 and remade
nothing.

Only a shelf's owner can mark it to sync to the Kobo, and the Kobo API creates
shelves unmarked, so a shelf-only account's collections would never reach its
device. For accounts setup made, the shim knows the CWA password and creates the
shelf through the web form first; the Kobo API's create then returns it by name.
A configured account syncs the whole library, where unmarked shelves still arrive
as collections, unless it's given its CWA login (ACCOUNT_CWA_USER and
ACCOUNT_CWA_PASSWORD). Then the shim marks its existing collections to sync too,
finding each shelf's id by name in the page's sidebar, which must happen before
that account is switched to shelf-only sync or its Kobo empties.

A Kobo only hears a collection was deleted when it syncs with the account that
owned it. Moving a Kobo to another CWA account leaves the old account's
collections on the device, which the reader deletes by hand.

CWA marks its session cookie Secure when it enforces HTTPS or OAuth is on, and the
shim reaches it over plain HTTP in the pod, so sessions use a jar that sends the
cookies back anyway. A standard jar would drop them and logins would quietly fail.

The Kobo API's create returns the user's existing shelf of the same name, so the
sync makes names unique, and a person's own shelf with the same name is adopted.

CWA hides a user's Kobo token from a request whose Host is localhost
(cps/kobo_auth.py:76-90), and the shim reaches CWA as localhost in the pod, so its
web sessions send Host cwa.internal instead. The first in-cluster signup, on
2026-11-03, failed on exactly this. A signup that fails after creating the user
deletes that user, so a retry doesn't hit a taken name.

CWA's new-user form answers 200 whether or not it worked, ignoring the redirect
its handler returns (cps/admin.py:1671-1683), so the user is looked up afterwards.
Its refusal is in the page's alert, which is safe to log.

LazyLibrarian's addBook with source HardCover keeps the Hardcover id as its BookID
(seen 2026-11-03). For an author it doesn't have, the first call adds only the
author and answers with an HTML page, and the second adds the book, so the add is
tried twice and the id checked before queueing.

OpenLibrary answered 500 once on 2026-11-03, which ended that run's downloads
before one book's failure was made to skip only that book.

The OPDS feed has no identifiers, so CWA books match on title and author only.

The library list uses the claude-robot CWA account over OPDS basic auth. It can't
read anyone's status, which is why the Kobo token is needed as well.

## DECISION

CWA's Kobo state endpoint rather than writing app.db, 2026-11-03. It runs CWA's
own code, which updates the timestamp the Kobo sync checks (cps/ub.py:735).
Writing app.db from the sidecar would depend on CWA's schema and skip that hook.

Accounts keyed by the CWA token in the path, 2026-11-03, because every Kobo
request already carries it.

Cloudflare Access as the only login for setup, 2026-11-03. Access's policy is the
guest list, and the page acts only on the signed-in email. A second password would
add nothing against the one remaining risk, a stolen Access session.

All of a user's Want to Read is downloaded with no approval, 2026-11-03, Fabian's
call: ebooks are small and storage isn't a concern.

## VERIFY

    go test -race ./internal/accounts/ ./internal/cwasync/ ./internal/setup/ ./internal/wanted/

Live on 2026-11-03: the first run marked 8 books Finished with no errors. A probe
on throwaway CWA users created a collection, added, renamed, removed and deleted
it, and made a user, their Kobo token, a synced shelf and a collection in it, then
deleted the user.

Not yet seen live: the Kobo applying a Finished or a collection on its next sync,
whether it reports a Finished back, whether a Kobo deletes a downloaded book when
it leaves every synced collection, and LazyLibrarian's Hardcover source.

## SEE ALSO

- [arch-matching.md](arch-matching.md) - how the matches this relies on are made,
  and the writes the worker sends.
- [arch-kobo-traffic.md](arch-kobo-traffic.md) - what reaches CWA from the Kobo.
