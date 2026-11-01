---
id: arch-kobo-traffic
see_also:
  - arch-capture
---
# Kobo traffic through CWA

What the proxy path can see. Everything here describes CWA or the Kobo rather
than this repo, so no gate flags it when it goes stale. Claims are from reading
CWA v4.0.8 on 2026-11-01 unless marked otherwise. No real device traffic has been
captured yet.

## GOTCHA

The shim can't see store metadata fetched by GET. For a book CWA doesn't hold,
it answers GET with a 307 to Kobo's store and the device goes there directly. Only
non-GET requests are proxied by CWA itself, so a state PUT is visible but its
metadata lookup isn't (CWA cps/kobo.py, redirect_or_proxy_request).

Sync is incremental, so a book already on the device may never appear in a sync
response. An ID map built only from sync has holes. The planned fix is a metadata
lookup on the first unknown PUT, using that request's Kobo auth, in memory only.

CWA's sync entries don't appear to carry ISBNs: CWA cps/kobo.py never mentions
them. Its own books need title and author, or a read-only look at metadata.db
for the hardcover-id identifiers.

Not every Kobo call lives under the /kobo/ token path. CWA points
reading_services_host at the site root when annotation sync is on or the store
proxy is off, and the firmware then calls /api/v3 and /api/UserStorage at the root.
With the proxy on and annotation sync off they should stay with Kobo. A root call
would hit the Cloudflare Access login, so check this before the tunnel phase.

## DECISION

Disable CWA's Hardcover sync and have the shim own all writes, 2026-11-01, so
there is one matching path. The rejected alternative was leaving CWA to handle its
own books. Highlight sync is lost, and Fabian doesn't use it.

USB-sideloaded books are out of scope, 2026-11-01. They probably create no server
traffic, and supporting them would need something running on the device. Unverified.

## VERIFY

Unverified until the first capture session: whether store and Libby progress
really passes through CWA, whether those sync entries carry ISBNs, and which hosts
the Libra's firmware uses. Read the store, Libby and CWA books in that session and
check the capture file.

## SEE ALSO

- [arch-capture.md](arch-capture.md) - the proxy that collects the evidence these
  claims are waiting on, and the invariants any shim change has to keep.
