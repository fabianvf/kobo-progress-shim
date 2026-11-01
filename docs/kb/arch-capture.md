---
id: arch-capture
covers:
  - cmd/kobo-capture/**
  - internal/capture/**
see_also:
  - arch-kobo-traffic
---
# Capture proxy

Phase 0 of the Kobo to Hardcover sync. It sits in front of CWA and records what
the Kobo sends, so the spec can be written from real traffic.

## INVARIANT

Capture never changes or delays Kobo traffic. If it does, a sync fails on a
device with no useful error, and the only recovery is editing its config over USB.
Hence the non-blocking queue, the writer that never errors, and a recorder that
starts even when its file can't be opened.

Host, the raw query and X-Forwarded-* reach CWA as received. CWA builds absolute
download and cover URLs from them, so losing them sends the Kobo to the wrong host.

The transport must not add Accept-Encoding. It would change what upstream
returns, and Go would then transparently decode it, hiding the real response.

Secrets stay out of the capture file. Headers use an allowlist so an auth header
nobody has seen yet is redacted by default. A JSON body that fails to parse is
dropped, because it can't be redacted. CWA's token is a path segment that also
appears inside returned download URLs, so it is scrubbed from every string, not
only the request line.

The file's size cap must stay under the volume's sizeLimit. Breaching sizeLimit
evicts the pod, which takes down the Kobo's path along with the capture.

## DECISION

Go rather than Python, 2026-11-01: a static binary on a distroless image is
simpler to build and deploy, and nothing here needs a Python library.

Standalone sidecar rather than a CWA PR, 2026-11-01. CWA calls Hardcover inline
during the Kobo's PUT with no timeout (CWA cps/services/hardcover.py, v4.0.8),
which breaks the invariant above, and fixing that means adding a durable queue and
review UI to a project with 146 open PRs. A sidecar also works unchanged with the
Calibre-Web-NextGen fork.

## VERIFY

    go test -race ./...

## SEE ALSO

- [arch-kobo-traffic.md](arch-kobo-traffic.md) - owns the claims about which
  requests pass through CWA, which decide what capture can and can't see.
