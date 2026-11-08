# Runbook: failed reservation messages

Use this when an alarm email arrives from the `pokedrop-<stage>` alarm topic, or when reservations
stay `QUEUED` (the dashboard shows **Delayed**). The steps are: inspect → fix the cause → remove
poison messages → replay → verify.

Commands are for `pokedrop-dev`. Set `STAGE` for another stage. Every command is read-only except
those marked **write**: they change queue data (not resources), so double-check them before
running. On an `ExpiredToken` error, run `aws login --profile pokedrop` and start again.

## Alarms

| Alarm | Fires when | Usual cause |
|---|---|---|
| `pokedrop-dev-dlq-messages` | Any message is visible in the dead letter queue | A message failed all 5 receives |
| `pokedrop-dev-worker-error-logs` | The worker logs an `ERROR` line | A message failed permanently (malformed), or the worker failed to start |
| `pokedrop-dev-queue-backlog-age` | The oldest queued message stays ≥ 300 s old for 3 minutes | The worker is down, throttled or failing everything |
| `pokedrop-dev-worker-crashes` | A worker invocation errors, times out or is throttled | A panic, timeout or startup failure (per-record failures do not count) |

Every alarm emails again when it returns to OK. Missing data counts as OK: SQS stops publishing
metrics for an idle queue, so a DLQ alarm can take several minutes to fire or to clear.

## 0. Set up the shell

```sh
export AWS_PROFILE=pokedrop AWS_REGION=us-west-2 STAGE=${STAGE:-dev}
out() { aws cloudformation describe-stacks --stack-name "pokedrop-$STAGE" \
  --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue | [0]" --output text; }
QUEUE_URL=$(out ReservationQueueUrl); DLQ_URL=$(out ReservationDlqUrl); DLQ_ARN=$(out ReservationDlqArn)
WORKER_LOGS=$(out WorkerLogGroupName); INVENTORY=$(out InventoryTableName)
RESERVATIONS=$(out ReservationsTableName); API=$(out ApiUrl)
echo "$QUEUE_URL $DLQ_URL $WORKER_LOGS"   # none of these should be "None"
```

## 1. Inspect

**Alarm state:**

```sh
aws cloudwatch describe-alarms --alarm-name-prefix "pokedrop-$STAGE-" \
  --query "MetricAlarms[].[AlarmName,StateValue,StateUpdatedTimestamp]" --output table
```

**Queue depth** (visible = waiting, not visible = being processed or waiting out a retry):

```sh
for q in "$QUEUE_URL" "$DLQ_URL"; do
  aws sqs get-queue-attributes --queue-url "$q" --output text \
    --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
    --query "[Attributes.ApproximateNumberOfMessages, Attributes.ApproximateNumberOfMessagesNotVisible]"
done
```

**Worker logs.** The worker logs one JSON line per record, with `messageId`, `requestId`, `cardId`,
`attempt` (the SQS receive count), `outcome` and `durationMs`. `outcome` is `CONFIRMED` or
`SOLD_OUT` (with `duplicate`), `RETRY` (WARN) or `FAILED` (ERROR). A line with `injectedFailure`
means the dev failure flag is on.

```sh
# Errors (permanent failures, startup errors) in the last hour
aws logs tail "$WORKER_LOGS" --since 1h --format short --filter-pattern '{ $.level = "ERROR" }'
# Everything that will be retried, including injected failures
aws logs tail "$WORKER_LOGS" --since 1h --format short --filter-pattern '{ $.level = "WARN" }'
# The full history of one request
REQUEST_ID=<requestId>
aws logs tail "$WORKER_LOGS" --since 1d --format short --filter-pattern "{ \$.requestId = \"$REQUEST_ID\" }"
# Crashes and timeouts are plain-text Lambda lines, not JSON
aws logs tail "$WORKER_LOGS" --since 1h --format short --filter-pattern '?"Task timed out" ?panic ?"Runtime exited"'
```

**What's in the DLQ.** This hides each returned message for 10 s, so one call can't return the same
message twice. With `--visibility-timeout 0`, a message is visible again at once, and a single call
can return 10 copies of one message and none of the others. SQS returns only a sample of messages
per call, so wait 10 s and repeat until you have seen as many distinct IDs as the queue depth.
Receiving from the DLQ is harmless: the DLQ has no redrive policy of its own.

```sh
aws sqs receive-message --queue-url "$DLQ_URL" --max-number-of-messages 10 \
  --visibility-timeout 10 --wait-time-seconds 5 --attribute-names All \
  --query "Messages[].{id:MessageId,firstSent:Attributes.SentTimestamp,body:Body}" --output table
```

Look up each message's `requestId` in the logs (above). The last line before it reached the DLQ
says why it failed.

## 2. Classify and fix the cause

| Last log line for the message | Class | Fix | Then |
|---|---|---|---|
| `FAILED`, `body is not a reservation message`, `not a UUID`, `no trainerId` or `invalid cardId` | Poison: it can never succeed | None | Delete it (step 3). Replaying it only fails 5 more times |
| `FAILED`, card missing | Valid shape, unknown card | Seed the card if it should exist (`make seed`), otherwise treat it as poison | Replay or delete |
| `RETRY` with a conflict, throttling or timeout error on every attempt | Transient | Wait for the outage to end, or fix the throttling or capacity issue | Replay (step 4) |
| `CONFIRMED`/`SOLD_OUT` with `injectedFailure` | The dev failure flag was left on | `make fail-status`, then `make fail-off` (after `cdk diff`) and `make fail-status` again prints `off` | Replay |
| A crash or timeout, or `worker-crashes` in ALARM | Worker bug or bad deploy | Fix the code and deploy (`cdk diff`, then `make deploy`) | Replay |

**Replaying a valid message is always safe.** The worker writes the outcome only if none exists for
`(trainerId, requestId)`, and it takes stock in the same transaction. A request that already
committed comes back as a duplicate, and stock doesn't change. Fix the cause first, otherwise
the replayed message just fails 5 more times and returns to the DLQ.

## 3. Remove poison messages

For each poison message ID from step 1. This receive hides every returned message for 60 s.
If `HANDLE` prints `None`, the message wasn't returned this time: wait 60 s and run it again.

```sh
POISON_ID=<MessageId>
HANDLE=$(aws sqs receive-message --queue-url "$DLQ_URL" --max-number-of-messages 10 \
  --visibility-timeout 60 --wait-time-seconds 5 \
  --query "Messages[?MessageId=='$POISON_ID'].ReceiptHandle | [0]" --output text); echo "$HANDLE"
aws sqs delete-message --queue-url "$DLQ_URL" --receipt-handle "$HANDLE"   # write
```

Only if **every** message in the DLQ is poison, you can delete them all at once instead (this
can't be undone and can take up to 60 s):

```sh
aws sqs purge-queue --queue-url "$DLQ_URL"   # write
```

## 4. Replay

Before replaying, note each valid message's `requestId`, `trainerId` and `cardId` (from its body in
step 1) and that card's current stock (step 5), so you can compare afterwards.

Wait until no DLQ message is hidden by step 3 (`ApproximateNumberOfMessagesNotVisible` is `0`),
then move everything back to the queue it came from (no destination = the original source
queue):

```sh
aws sqs start-message-move-task --source-arn "$DLQ_ARN"   # write
aws sqs list-message-move-tasks --source-arn "$DLQ_ARN" --max-results 1 --output text \
  --query "Results[0].[Status,ApproximateNumberOfMessagesMoved,ApproximateNumberOfMessagesToMove,FailureReason]"
```

Repeat the second command until the status is `COMPLETED`, then once more: a read right after the
task finishes can show `COMPLETED` with `0` moved before the count catches up. Only one move task
can run per queue. Moved messages are new messages with new message IDs and start again at
`attempt` 1, so look them up by `requestId`, not `messageId`. The worker picks them up within
seconds.

## 5. Verify

**Both queues are empty** (all four numbers are `0`; run the queue depth command from step 1).

**The request reached a terminal outcome.** The last line is INFO with `outcome` `CONFIRMED` or
`SOLD_OUT`. `duplicate: true` means it had already committed before failing, so the replay
changed nothing:

```sh
aws logs tail "$WORKER_LOGS" --since 15m --format short --filter-pattern "{ \$.requestId = \"$REQUEST_ID\" }"
```

**The reservation is stored once.** The table key is `(trainerId, requestId)`, so there is at most
one row:

```sh
TRAINER_ID=<trainerId from the message body>
aws dynamodb get-item --table-name "$RESERVATIONS" --consistent-read \
  --key "{\"trainerId\":{\"S\":\"$TRAINER_ID\"},\"requestId\":{\"S\":\"$REQUEST_ID\"}}" \
  --query "Item.[cardId.S,status.S,updatedAt.S]" --output text
# The owner sees the same result through the API (another trainer gets 404)
curl -s -H "Authorization: Bearer $(make -s token)" "$API/reservations/$REQUEST_ID"; echo
```

**Stock moved by at most one per request.** Compare with the value noted before the replay. It's
unchanged when the replay was a duplicate, one lower when this replay was the first to commit,
and never negative:

```sh
CARD_ID=<cardId>
aws dynamodb get-item --table-name "$INVENTORY" --consistent-read \
  --key "{\"cardId\":{\"S\":\"$CARD_ID\"}}" --query "Item.stock.N" --output text
```

**Alarms are back to OK** (the alarm state command in step 1). The DLQ alarm clears once SQS
reports 0 visible messages, which can take a few minutes. Each alarm sends an OK email. In dev,
also check that `make fail-status` prints `off`.

## Drills (dev only)

| Drill | Commands | Expected |
|---|---|---|
| Failure after commit, retried | `make fail-on MODE=once`, one reservation, `make fail-off` | `attempt 1` logs `injectedFailure: once`; about 60 s later the next attempt logs `duplicate: true`. One row, stock −1 |
| Committed request dead-lettered and replayed | `make fail-on MODE=always`, one reservation, wait for the DLQ (~5 min), `make fail-off`, then steps 1–5 | The DLQ alarm fires. The replay logs `duplicate: true` and stock is unchanged by it |
| Poison message | `scripts/poison-message.sh` (prints the command), then `--send` | 5 `FAILED` ERROR lines about 60 s apart, then the message is in the DLQ. `dlq-messages` and `worker-error-logs` fire. Delete it (step 3) |

`make fail-on`/`fail-off` are full CDK deploys, so run `cdk diff` first (with
`-c failAfterCommit=…`). CDK refuses the flag outside dev, and a plain `make deploy` always turns
it off.

## Drill results

### Failure after commit, retried (`pokedrop-dev`, 2026-11-08)

`make fail-on MODE=once`, then one `POST /reservations` for `card-001` with request ID
`14c20615-81b7-4688-97d2-34c322172940` (202 `QUEUED` at 23:04:05 UTC). The worker log lines for that
request:

```json
{"time":"2026-11-08T23:04:08.56460867Z","level":"WARN","msg":"injected failure after commit, message will be retried","messageId":"1c04c6fb-c18e-453d-9f4b-f35a85d876fe","requestId":"14c20615-81b7-4688-97d2-34c322172940","cardId":"card-001","attempt":1,"outcome":"CONFIRMED","duplicate":false,"injectedFailure":"once","durationMs":638}
{"time":"2026-11-08T23:05:07.706276363Z","level":"INFO","msg":"reservation processed","messageId":"1c04c6fb-c18e-453d-9f4b-f35a85d876fe","requestId":"14c20615-81b7-4688-97d2-34c322172940","cardId":"card-001","attempt":2,"outcome":"CONFIRMED","duplicate":true,"durationMs":20}
```

Attempt 1 stored the outcome and took the stock, then failed before reporting success. SQS
redelivered the message after the 60 s visibility timeout. Attempt 2 found the stored outcome and
returned it without touching inventory.

| Check | Result |
|---|---|
| `card-001` stock (consistent read) | 9 before, 8 after: one decrement |
| Reservation rows with this request ID, across all trainers (consistent scan) | 1, `CONFIRMED`, `updatedAt` 2026-11-08T23:04:07Z |
| `GET /reservations/14c20615-…` as the owner | `CONFIRMED` |
| Queue and DLQ | 0 visible, 0 in flight |
| Alarms | All OK. The injected failure logs WARN, so it does not trip `worker-error-logs` |

Then `make fail-off`, and `make fail-status` printed `off`.

### Poison message (`pokedrop-dev`, 2026-11-08)

`scripts/poison-message.sh --send` at 23:27:31 UTC: message `225d1a96-f107-4737-82f2-ee07ea87bf46`,
request ID `poison-20261108T232731Z`.

| Check | Result |
|---|---|
| Receives | 5 × ERROR `FAILED` (`requestId "poison-20261108T232731Z" is not a UUID`), at 23:27:32, 23:28:32, 23:29:32, 23:30:32 and 23:31:32: exactly the 60 s visibility timeout apart |
| DLQ | 1 message by 23:33; main queue 0 visible, 0 in flight |
| `worker-error-logs` | OK → ALARM at 23:28:29 (after the first ERROR line); SNS publish succeeded; email received |
| `dlq-messages` | OK → ALARM at 23:36:06, about 4.5 min after the last receive (SQS metric lag); SNS publish succeeded; email received |
| `queue-backlog-age` | Stayed OK: one message's retry cycle stays under the 300 s threshold |
| `worker-error-logs` recovery | Back to OK on its own at 23:38:29, 10 min after alarming and about 7 min after the last ERROR line, with no action (missing data is not breaching) |

### Committed request dead-lettered, sorted and replayed (`pokedrop-dev`, 2026-11-08)

The poison message was left in the DLQ, so the runbook had to sort a mixed DLQ. `make fail-on
MODE=always`, then one `POST /reservations` for `card-001` with request ID
`b851abc2-01a8-40d4-bebc-9042cb027149` at 23:39:14 UTC (stock 8 before).

- **Dead-lettered:** attempt 1 logged `CONFIRMED`, `duplicate: false`, and attempts 2–5 logged
  `duplicate: true`, all WARN with `injectedFailure: always`. By 23:44:41 the DLQ held 2 messages
  (the main queue was 0/0), and stock was 7.
- **Inspect (step 1):** the first peek, with `--visibility-timeout 0`, returned the poison message
  10 times and never the valid one. The runbook now peeks with `--visibility-timeout 10`, which
  found `9a3696b7-eb2e-4c41-a2c7-5911873938e0` (the `b851abc2-…` body, 6 receives).
- **Sort (step 2):** `b851abc2-…` = `CONFIRMED` with `injectedFailure`, so the flag was left on:
  `make fail-off`, then `make fail-status` printed `off`. `poison-…` = `FAILED`, not a UUID, so poison.
- **Delete (step 3):** the first receive didn't return the poison message (`HANDLE` was `None`, so
  nothing was deleted). After the 60 s visibility timeout, the same command deleted it at 23:51:34.
  The DLQ then showed 1 visible / 0 in flight, and a peek returned only `9a3696b7-…`.
- **Replay (step 4):** `start-message-move-task` at 23:52:46; `COMPLETED`, 1 of 1 moved. The
  first status read said 0 moved.

The worker log line for the replayed message:

```json
{"time":"2026-11-08T23:52:48.261830002Z","level":"INFO","msg":"reservation processed","messageId":"a3d3926c-0899-4ee5-90a7-b21fb27b7581","requestId":"b851abc2-01a8-40d4-bebc-9042cb027149","cardId":"card-001","attempt":1,"outcome":"CONFIRMED","duplicate":true,"durationMs":504}
```

| Check (step 5) | Result |
|---|---|
| Replay outcome | `duplicate: true`, new message ID, `attempt` 1 (the receive count starts over) |
| `card-001` stock | 8 before the request, 7 after attempt 1, still 7 after the replay: one decrement over 6 deliveries |
| Reservation rows with this request ID, across all trainers | 1, `CONFIRMED`, `updatedAt` 2026-11-08T23:39:18Z (the original commit) |
| `GET /reservations/b851abc2-…` as the owner | `CONFIRMED` |
| Queue and DLQ | 0 visible, 0 in flight |
| Alarms | `dlq-messages` back to OK at 23:55:06, about 2.3 min after the DLQ emptied (it had stayed in ALARM since 23:36:06, so the second dead letter sent no new email); the OK notification was published. All four alarms OK |
