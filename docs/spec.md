# Spec: Kobo progress to Hardcover

Status: signed off 2026-11-02. Based on the interview and one capture session on
2026-11-02 (Kobo N873, firmware 4.38.23852, CWA v4.0.8, 56 Kobo requests).

## Goal

Reading on the Kobo updates Hardcover with no extra steps on the device. This
covers CWA books, Libby books and store books.

## What the capture showed

Progress arrives as `PUT /kobo/<token>/v1/library/<EntitlementId>/state`, through
the proxy, for both a CWA book and a Libby book. Fields used:

    ReadingStates[].EntitlementId
    ReadingStates[].CurrentBookmark.ProgressPercent
    ReadingStates[].CurrentBookmark.Location.Source
    ReadingStates[].StatusInfo.Status              Reading | Finished
    ReadingStates[].StatusInfo.LastModified

Identity differs by source:

- CWA book: `library/sync` returns `NewEntitlement.BookMetadata` with title and
  contributors, keyed by the same id as the PUT. `ExternalIds` is empty, so there
  is no ISBN.
- Libby book already on the device: nothing in the log gives its title. The bookmark
  path sometimes holds an ISBN, in 17 of 74 books checked, so it can only support a
  match. The store lookup below supplies the rest.
- Analytics events carry `Origin: OverDrive` and a `volumeid` that differs from
  the EntitlementId. Nothing links the two ids directly, so these aren't used.

## Behaviour

Traffic: the proxy forwards everything unmodified. A copy of each state PUT with
a 2xx response goes on a non-blocking queue. A failure in anything below never
reaches the Kobo.

Identifying a book uses only what the proxy can see live. `KoboReader.sqlite` is
never an input. It is the answer key for measuring the matcher.

Signals, gathered in this order for an EntitlementId not yet resolved:

1. `hardcover-id` on the book in CWA's metadata.db, for CWA books.
2. ISBN-13 with a valid check digit in `Location.Source`.
3. ISBN, title and author from a sync entry seen for this id.
4. Store lookup at `/v1/library/<id>/metadata`, using the auth from that PUT, held
   in memory only and dropped after the call.

Deciding a match:

- Automatic: a `hardcover-id` is present, or an ISBN finds one Hardcover book and
  the title and author agree, or there is no ISBN hit and search finds exactly one
  book with the same normalized title and author.
- Review queue, with the best candidate suggested: anything else, including an ISBN
  with no title to check it against, several exact matches, and bundles. Nothing is
  written to Hardcover until it is confirmed.

A resolved match is saved against the EntitlementId. A match confirmed by hand
replays the latest progress.

Measuring it: `cmd/match-eval` runs the matcher on the answer key's books with only
the signals a scenario allows, and reports automatic, review and wrong counts. A
wrong automatic match is a failure to fix before live writes.

Status:

- `Finished` or 100%: read, with the finish date from `StatusInfo.LastModified`.
- `Reading` above 1%: currently reading, with progress.
- Otherwise: no write.
- A book already marked read stays read. Re-reads aren't tracked, so a book read
  on Hardcover gets no new read, even when it has no dated read.

Ratings:

- A rating on the Kobo sets the rating on Hardcover, once the book is matched.
- Only a new rating action writes. Syncs never touch a rating set on Hardcover.
- The request is `POST /v1/products/<id>/rating/<stars>`. Its response doesn't
  matter, because CWA answers 404 for its own books and the rating is still real.
- That id is the store's `RevisionId`, which differs from the EntitlementId on Libby
  books, so the store lookup also saves `RevisionId` to connect them.

Hardcover writes:

- One user_book per book and one read row per user_book, created if missing and
  updated after that, so repeated syncs can't create duplicates.
- Each write reads the book's current Hardcover state and sends only the difference,
  so an unchanged book sends nothing.
- Progress is sent as pages, because Hardcover's read input has no percent field.
  An edition with no page count gets the status but no progress.
- The latest state per book is saved in SQLite, marked pending until it reaches
  Hardcover, and retried every 5 minutes under the 60 requests a minute limit. A
  restart loses nothing. Superseded progress is never sent.
- `DRY_RUN=true` is the default and logs the intended write.
- No bulk mutation covers shelves or reads across books, so writes go one at a time.

Accounts:

- A user is the CWA sync token in the Kobo's path. `ACCOUNTS_FILE` lists each
  user's Kobo token and Hardcover token. Without it, `HARDCOVER_API_KEY` and an
  optional `KOBO_SYNC_TOKEN` and `ACCOUNT_EMAIL` make one account.
- Books are saved per user. A Kobo no account claims is saved but never written.

Hardcover to CWA, so the Kobo shows what Hardcover has as read:

- Hourly, CWA's library is listed over OPDS as the robot account, and each CWA
  book is matched like any other. Books needing review show on the activity page.
- A book read on Hardcover is set Finished through CWA's Kobo state API, with the
  user's token, unless the Kobo is partway through it. The Kobo picks it up on its
  next sync.
- Only a Finished the sync set is ever undone, when Hardcover no longer has the
  book as read. Progress is never copied. Store and Libby books aren't covered.
- Needs `CWA_USER`, `CWA_PASSWORD` and an account with a Kobo token. `DRY_RUN`
  applies.

Collections, downloads and setup:

- Each user's Hardcover shelves and lists become Kobo collections, plus one named
  Hardcover holding everything on any of them. Only what the sync added is changed.
- A book Hardcover has as being read gets Reading and its percent in CWA, only if
  the Kobo hasn't opened that copy.
- Want to Read books the library lacks go to LazyLibrarian one at a time, by
  Hardcover id with `LL_SOURCE=HardCover`, else through OpenLibrary. Needs
  `LL_URL` and `LL_API_KEY`. Nothing else asks LazyLibrarian for books.
- A setup page on `SETUP_ADDR` at `SETUP_PATH` (default `/setup/`), behind Cloudflare Access, takes a Hardcover key,
  makes the CWA user with shelf-only sync, and shows the Kobo's `api_endpoint`.
  Needs `ACCOUNTS_KEY`, `ACCESS_TEAM`, `ACCESS_AUD` and `PUBLIC_URL`.

Activity page, on the admin port: recent events, last write result per book, and
the review queue with a field for a Hardcover book id or URL. Nothing else on that
port is reachable from the Kobo's route.

Secrets: the Hardcover token comes from an env var or file. No Kobo auth or
Hardcover token is logged or stored.

## Build order

1. Parse state PUTs and sync entries behind the proxy, and save the id mapping.
2. Identification, the review queue and the activity page, with dry-run logging.
3. Hardcover client and writes, still dry-run. Check the real schema first.
4. Live writes on a test book, then on everything.
5. Backfill of reading history, reviewed as a list before anything is written.
6. Tunnel for remote sync, which needs one more `api_endpoint` edit, then re-capture.

## Not verified

- Store books. Expected to behave like Libby books, not seen yet.
- The Kobo applying a Finished the CWA sync set, and whether it reports it back.
- A CWA book's not-found response from the store lookup. The lookup itself works
  live: a Libby and a store book each returned ISBN, title and author.
- A newly added Libby or store book appearing in sync with metadata.
- A rating on a Libby or store book. Only a CWA book's rating has been seen, and its
  id matched its EntitlementId.
- Hardcover's rate limits. The docs page returned 403 and the limit is from a search
  excerpt. A burst of about 15 requests got a 429.
- Whether `/v1/initialization` listing `library_sync` at storeapi.kobo.com matters.
  The device still sent its syncs through the proxy in this session.

## Out of scope

Highlights, audiobooks, USB sideloads and re-read counts.
