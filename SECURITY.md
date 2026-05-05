# Security policy

## Reporting a vulnerability

If you've found a security issue in quadstore — anything that lets an
attacker bypass the label namespace enforcement, corrupt the underlying
SQLite store, exfiltrate data across the `human:{tenant}/...` boundary,
or otherwise violate the library's stated guarantees — please report it
privately first.

**How to report:**

- GitHub: open a [private security advisory](https://github.com/dukkandcards/quadstore/security/advisories/new).
- Email: open an issue saying "I have a security report; please give me a contact." We'll respond with a private channel.

Do **not** open a public issue for a security vulnerability before
we've had a chance to look at it. We'll respond as quickly as we can,
acknowledge receipt within 72 hours, and aim to ship a fix within
14 days for high-severity issues.

## What's in scope

- The label-namespace enforcement (`source:` / `derived:` / `human:` / `meta:` prefixes).
- Cross-partition isolation in `OpenPartitioned` mode.
- The `Migrate` / `MigrateFromSnapshot` correctness guarantees (idempotency, source-immutability).
- SQL injection or unsafe parameter handling in any quadstore-emitted query.
- Resource exhaustion in well-formed inputs (e.g. a Pattern that triggers a runaway memory allocation).

## What's not in scope

- Vulnerabilities in `modernc.org/sqlite` itself — please report those upstream.
- Vulnerabilities in your application's use of quadstore (e.g. you constructed a label from untrusted user input — that's an application bug). We can advise but the responsibility is upstream.
- Issues that require attacker control of the SQLite file on disk. If an attacker can write to your DB file, they own everything in it; that's outside the library's threat model.

## Disclosure

We follow coordinated disclosure: report → fix → public advisory + CVE
if applicable. We'll credit reporters in the advisory and the
CHANGELOG unless you'd rather stay anonymous.
