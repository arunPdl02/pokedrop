#!/usr/bin/env bash
# Put one malformed reservation message straight on the reservation queue, to check that a poison
# message reaches the dead letter queue after 5 receives and raises the DLQ alarm. The API
# validates every request, so a malformed message can only arrive this way.
#
#   scripts/poison-message.sh          # print the exact send command, change nothing
#   scripts/poison-message.sh --send   # send it
#
# The body is valid JSON with a requestId that is not a UUID, so the worker logs it with a
# findable requestId (poison-<UTC timestamp>) and fails it permanently on every receive. It never
# touches inventory. Remove it from the dead letter queue afterwards (docs/runbook.md); do not
# replay it. This is a data write to a dev queue, not a resource change. It refuses prod.

set -euo pipefail

STAGE="${STAGE:-dev}"
export AWS_PROFILE="${AWS_PROFILE:-pokedrop}"
export AWS_REGION="${AWS_REGION:-us-west-2}"

die() { echo "poison-message: $*" >&2; exit 1; }

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

SEND=false
case "${1:-}" in
  "") ;;
  --send) SEND=true ;;
  *) usage ;;
esac
[ $# -le 1 ] || usage

[ "$STAGE" != "prod" ] || die "refusing to send a poison message to prod"

err="$(mktemp)"
trap 'rm -f "$err"' EXIT
if ! queue_url="$(aws cloudformation describe-stacks --stack-name "pokedrop-$STAGE" \
  --query "Stacks[0].Outputs[?OutputKey=='ReservationQueueUrl'].OutputValue | [0]" \
  --output text 2>"$err")"; then
  cat "$err" >&2
  if grep -q ExpiredToken "$err"; then
    echo "AWS credentials have expired: run \`aws login --profile pokedrop\` and retry" >&2
  fi
  die "could not read the ReservationQueueUrl output of pokedrop-$STAGE"
fi
{ [ -n "$queue_url" ] && [ "$queue_url" != "None" ]; } || die "pokedrop-$STAGE has no ReservationQueueUrl output"

body="{\"requestId\":\"poison-$(date -u +%Y%m%dT%H%M%SZ)\",\"trainerId\":\"poison-test\",\"cardId\":\"card-001\",\"poison\":true}"
cmd=(aws sqs send-message --queue-url "$queue_url" --message-body "$body")

# The body has no single quotes, so this prints a command that can be pasted as is.
echo "AWS_PROFILE=$AWS_PROFILE AWS_REGION=$AWS_REGION aws sqs send-message --queue-url '$queue_url' --message-body '$body'"
if [ "$SEND" = true ]; then
  "${cmd[@]}"
else
  echo "(not sent; rerun with --send)" >&2
fi
