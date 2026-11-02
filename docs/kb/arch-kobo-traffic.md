---
id: arch-kobo-traffic
see_also:
  - arch-capture
---
# Kobo traffic through CWA

What the proxy path can see. Everything here describes CWA or the Kobo rather
than this repo, so no gate flags it when it goes stale. Claims are from reading
CWA v4.0.8 on 2026-11-01, and from one capture on 2026-11-02 (N873, firmware
4.38.23852, CWA and Libby books, no store book read).

## GOTCHA

The shim can't see store metadata fetched by GET. For a book CWA doesn't hold,
it answers GET with a 307 to Kobo's store and the device goes there directly. Only
non-GET requests are proxied by CWA itself, so a state PUT is visible but its
metadata lookup isn't (CWA cps/kobo.py, redirect_or_proxy_request).

Sync is incremental, so a book already on the device may never appear in a sync
response. Seen: a Libby book's state came back through sync with no title or
metadata. See arch-matching for the store lookup that fills this gap.

The state PUT carries an EntitlementId, which isn't the id analytics events call
volumeid, and nothing in the traffic links them.

CWA's sync entries carry a title and contributors with empty ExternalIds. Its own
books need title and author, or a read-only look at metadata.db for the
hardcover-id identifiers.

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

Seen in the capture: Libby and CWA progress both pass through as state PUTs. Store
progress is still unverified, and so is a new store or Libby book arriving in sync.
Read one and check the capture file.

## SEE ALSO

- [arch-capture.md](arch-capture.md) - the proxy that collects the evidence these
  claims are waiting on, and the invariants any shim change has to keep.
