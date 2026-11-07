#!/usr/bin/env bash
# Add or remove one fixture reservation so the ownership rule can be checked against a deployed
# dev stack before the Phase 3 worker exists (only the worker writes real outcomes).
#
#   scripts/ownership-fixture.sh add      # CONFIRMED reservation owned by the user `make token` signs in
#   scripts/ownership-fixture.sh remove   # delete that fixture (only ever deletes items marked fixture)
#
# The owner is whoever `make token` resolves to (TEST_USER_EMAIL / TEST_USER_PASSWORD from the
# environment or .env.local). To check ownership, add the fixture as trainer A, then:
#
#   API=<ApiUrl output>; ID=ffffffff-0000-4000-8000-000000000001
#   A=$(make -s token); B=$(TEST_USER_EMAIL=<second user> make -s token)
#   curl -H "Authorization: Bearer $A" "$API/reservations/$ID"   # 200 CONFIRMED
#   curl -H "Authorization: Bearer $B" "$API/reservations/$ID"   # 404 RESERVATION_NOT_FOUND
#
# This is a data write to a dev table (like `make seed`), not a resource change. It refuses prod.
# Run it from anywhere; it needs a deployed stack and valid AWS credentials.

set -euo pipefail

FIXTURE_REQUEST_ID="ffffffff-0000-4000-8000-000000000001"
FIXTURE_CARD_ID="card-001"

STAGE="${STAGE:-dev}"
export AWS_PROFILE="${AWS_PROFILE:-pokedrop}"
export AWS_REGION="${AWS_REGION:-us-west-2}"

die() { echo "ownership-fixture: $*" >&2; exit 1; }

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

[ $# -eq 1 ] || usage
ACTION="$1"
case "$ACTION" in add | remove) ;; *) usage ;; esac

[ "$STAGE" != "prod" ] || die "refusing to write fixtures to prod"

cd "$(dirname "$0")/.."

# Run an aws command in the current shell (not a subshell) so its result survives: stdout goes to
# AWS_OUT and stderr to AWS_ERR.
AWS_OUT=""
AWS_ERR=""
aws_run() {
  local out err rc=0
  out="$(mktemp)"
  err="$(mktemp)"
  "$@" >"$out" 2>"$err" || rc=$?
  AWS_OUT="$(cat "$out")"
  AWS_ERR="$(cat "$err")"
  rm -f "$out" "$err"
  return "$rc"
}

# Report the last failed aws call, with the sign-in hint when the credentials expired, and exit.
fail_aws() {
  echo "$AWS_ERR" >&2
  case "$AWS_ERR" in
    *ExpiredToken*) echo "AWS credentials have expired: run \`aws login --profile pokedrop\` and retry" >&2 ;;
  esac
  die "$1"
}

aws_run aws cloudformation describe-stacks --stack-name "pokedrop-$STAGE" \
  --query "Stacks[0].Outputs[?OutputKey=='ReservationsTableName'].OutputValue | [0]" --output text ||
  fail_aws "could not read the ReservationsTableName output of pokedrop-$STAGE"
table="$AWS_OUT"
{ [ -n "$table" ] && [ "$table" != "None" ]; } || die "pokedrop-$STAGE has no ReservationsTableName output"

# The trainer ID is the token's sub claim. The token is passed to python on stdin, not argv, and
# is never printed.
token="$(make -s token)" || die "make token failed"
trainer="$(printf %s "$token" | python3 -I -c '
import base64, json, sys
payload = sys.stdin.read().split(".")[1]
payload += "=" * (-len(payload) % 4)
print(json.loads(base64.urlsafe_b64decode(payload))["sub"])
')"
unset token
[ -n "$trainer" ] || die "could not read sub from the token"

key="{\"trainerId\":{\"S\":\"$trainer\"},\"requestId\":{\"S\":\"$FIXTURE_REQUEST_ID\"}}"

case "$ACTION" in
  add)
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    item="{\"trainerId\":{\"S\":\"$trainer\"},\"requestId\":{\"S\":\"$FIXTURE_REQUEST_ID\"},\"cardId\":{\"S\":\"$FIXTURE_CARD_ID\"},\"status\":{\"S\":\"CONFIRMED\"},\"updatedAt\":{\"S\":\"$now\"},\"fixture\":{\"BOOL\":true}}"
    if aws_run aws dynamodb put-item --table-name "$table" --item "$item" \
      --condition-expression "attribute_not_exists(requestId)"; then
      echo "added fixture $FIXTURE_REQUEST_ID for trainer $trainer in $table"
    elif [[ "$AWS_ERR" == *ConditionalCheckFailedException* ]]; then
      echo "fixture $FIXTURE_REQUEST_ID already exists for trainer $trainer in $table (nothing changed)"
    else
      fail_aws "put-item failed"
    fi
    ;;
  remove)
    # The condition guarantees only items marked as fixtures can ever be deleted.
    if aws_run aws dynamodb delete-item --table-name "$table" --key "$key" \
      --condition-expression "fixture = :t" --expression-attribute-values '{":t":{"BOOL":true}}'; then
      echo "removed fixture $FIXTURE_REQUEST_ID for trainer $trainer from $table"
    elif [[ "$AWS_ERR" == *ConditionalCheckFailedException* ]]; then
      echo "no fixture $FIXTURE_REQUEST_ID for trainer $trainer in $table (nothing removed)"
    else
      fail_aws "delete-item failed"
    fi
    ;;
esac
