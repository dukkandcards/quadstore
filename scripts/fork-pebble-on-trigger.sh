#!/usr/bin/env bash
# fork-pebble-on-trigger.sh — execute the Pebble relicensing mitigation.
#
# Run this script when (and only when) Cockroach Labs announces that
# Pebble is moving off a permissive license. It executes the contingency
# plan documented in docs/CONTINGENCY.md.
#
# What it does:
#   1. Clones cockroachdb/pebble at the last known-Apache commit
#      (pinned SHA below) into a local working directory.
#   2. Adds a `replace` directive to quadstore's go.mod pointing at
#      that local clone — proves quadstore builds + tests pass against
#      the fork before we publish it anywhere.
#   3. Reports next steps for publishing to dukkandcards/pebble.
#
# Re-runnable. Safe to invoke before any actual relicense (and useful
# as a periodic mitigation drill) — the script doesn't touch any
# remote state, only the local clone + this repo's go.mod.
#
# Verified end-to-end against the v2.1.5 freeze point on 2026-05-05.

set -euo pipefail

# --- pinned freeze point ----------------------------------------------------

# v2.1.5 — Pebble's last commit under BSD-3 / Apache-2.0 family licenses
# as of 2026-05-05. Update this only after re-verifying via the
# quarterly recheck procedure in docs/LICENSE_AUDIT.md.
PEBBLE_TAG="v2.1.5"
PEBBLE_SHA="36a5551312e40777b3afff9846796aaadca5f877"

# --- config -----------------------------------------------------------------

WORK_DIR="${WORK_DIR:-$HOME/quadstore-pebble-fork}"
REPO="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
UPSTREAM="https://github.com/cockroachdb/pebble.git"

# --- preflight --------------------------------------------------------------

if [[ ! -f "$REPO/go.mod" ]]; then
  echo "fork-pebble: can't find quadstore go.mod at $REPO" >&2
  exit 1
fi

# Refuse to clobber an existing replace directive.
if grep -qE "^replace github\.com/cockroachdb/pebble" "$REPO/go.mod"; then
  echo "fork-pebble: go.mod already has a replace for cockroachdb/pebble — refusing to overwrite" >&2
  echo "             remove the existing replace if you want to start fresh" >&2
  exit 1
fi

# --- clone -----------------------------------------------------------------

echo "==> cloning Pebble $PEBBLE_TAG ($PEBBLE_SHA) → $WORK_DIR"
if [[ -d "$WORK_DIR" ]]; then
  echo "    $WORK_DIR exists; updating to pinned SHA"
  git -C "$WORK_DIR" fetch --quiet --tags origin
  git -C "$WORK_DIR" checkout --quiet "$PEBBLE_SHA"
else
  git clone --quiet --depth 1 --branch "$PEBBLE_TAG" "$UPSTREAM" "$WORK_DIR"
fi

# Verify SHA matches the pinned value — refuse if upstream tag was moved.
ACTUAL_SHA=$(git -C "$WORK_DIR" rev-parse HEAD)
if [[ "$ACTUAL_SHA" != "$PEBBLE_SHA" ]]; then
  echo "fork-pebble: SHA mismatch! upstream $PEBBLE_TAG resolves to $ACTUAL_SHA, expected $PEBBLE_SHA" >&2
  echo "             upstream tag may have been moved — DO NOT proceed without investigating" >&2
  exit 2
fi
echo "    SHA verified: $ACTUAL_SHA"

# Verify license still permissive at this commit.
LICENSE_FIRST_LINE=$(head -1 "$WORK_DIR/LICENSE" || echo "")
if ! echo "$LICENSE_FIRST_LINE" | grep -qE "Copyright.*LevelDB-Go|Apache License|BSD"; then
  echo "fork-pebble: LICENSE first line unexpected: $LICENSE_FIRST_LINE" >&2
  echo "             license may have changed at this commit — investigate before proceeding" >&2
  exit 3
fi
echo "    license confirmed: $LICENSE_FIRST_LINE"

# --- retarget go.mod --------------------------------------------------------

echo "==> updating quadstore go.mod with replace directive"
(cd "$REPO" && go mod edit -replace="github.com/cockroachdb/pebble/v2=$WORK_DIR")
echo "    replace github.com/cockroachdb/pebble/v2 => $WORK_DIR"

(cd "$REPO" && go mod tidy >/dev/null 2>&1)

# --- verify -----------------------------------------------------------------

echo "==> running Pebble-related tests against local fork"
if (cd "$REPO" && go test -count=1 -run "Pebble|Crash|Migrate|Property" ./... >/tmp/fork-pebble-test.log 2>&1); then
  echo "    PASS — quadstore tests green against forked Pebble"
else
  echo "    FAIL — see /tmp/fork-pebble-test.log" >&2
  exit 4
fi

# --- next steps -------------------------------------------------------------

cat <<EOF

==> mitigation verified locally

Next steps (manual, operator decision):

  1. Push the local fork to GitHub:
       cd $WORK_DIR
       git remote rename origin upstream
       git remote add origin git@github.com:dukkandcards/pebble.git
       git push -u origin HEAD:main
       git push origin --tags

  2. Switch quadstore's replace directive to the GitHub fork:
       cd $REPO
       go mod edit -replace="github.com/cockroachdb/pebble/v2=github.com/dukkandcards/pebble/v2@$PEBBLE_SHA"
       go mod tidy

  3. Update docs:
       - docs/LICENSE_AUDIT.md: add row to Recheck log noting the fork date
       - docs/CONTINGENCY.md: change "if" → "did" for the Pebble section
       - CHANGELOG.md: note the fork rationale and date
       - README.md: brief mention so downstream users see it

  4. Tag a quadstore patch release marking the fork transition.

If this is a drill (no actual relicense yet), revert with:
       cd $REPO
       go mod edit -dropreplace=github.com/cockroachdb/pebble/v2
       go mod tidy
       rm -rf $WORK_DIR
EOF
