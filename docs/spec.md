# Spec: Kobo progress to Hardcover

Status: draft for sign-off. Based on the interview and one capture session on
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
- A book already marked read stays read. Re-reads aren't tracked.

Hardcover writes:

- One user_book per book and one read row per user_book, created if missing and
  updated after that, so repeated syncs can't create duplicates.
- Skip the call if status and whole-number progress haven't changed.
- Durable queue in SQLite with retry and backoff, under the 60 requests a minute
  limit. A restart loses nothing that was already queued.
- `DRY_RUN=true` is the default and logs the intended write.

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
- The store lookup with auth from a live PUT, and for a store book. It returned
  ISBN, title and author for three Libby loans, using the device's stored token.
- A newly added Libby or store book appearing in sync with metadata.
- Hardcover's schema and rate limits. The docs page returned 403 and the limit is
  from a search excerpt.
- Whether `/v1/initialization` listing `library_sync` at storeapi.kobo.com matters.
  The device still sent its syncs through the proxy in this session.

## Out of scope

Highlights, audiobooks, USB sideloads, re-read counts, and ratings. The capture did
show a rating event, so ratings could be added later.
