# PokéDrop

Pokémon card reservation app: trainers compete to reserve limited cards during simulated drops.
A Go backend on AWS processes reservations asynchronously and protects inventory under concurrent
requests and retries. See [docs/PLAN.md](docs/PLAN.md) for the plan and [docs/api.md](docs/api.md)
for the API contract.

Status: Phase 3 (inventory correctness). Trainers can sign in, list cards and queue a reservation;
a worker Lambda turns each queued request into `CONFIRMED` or `SOLD_OUT` with one conditional
DynamoDB transaction, so stock never goes negative and a repeated request ID never takes stock
twice. Messages that keep failing move to a dead letter queue after 5 receives. There is no UI yet
(Phase 4).

## Prerequisites

- Go (see `backend/go.mod`), Node.js 22+, GNU Make
- AWS CLI v2 with a profile named `pokedrop` (region `us-west-2`)
- The account/region bootstrapped for CDK (`CDKToolkit` stack in us-west-2)
- For `make token`: copy `.env.example` to `.env.local` (gitignored) and set `TEST_USER_EMAIL` and
  `TEST_USER_PASSWORD`. Nothing else needs it.

Sign in before deploying or seeding. If a command fails with `ExpiredToken`, run:

```sh
aws login --profile pokedrop
```

## Layout

```text
frontend/        placeholder until Phase 4
backend/         Go module: cmd/ (api Lambda, seed, token), internal/ (reservation rules, storage, queue)
infra/           CDK app (TypeScript)
tests/load/      load tests (Phase 6)
scripts/         helper scripts (ownership-fixture.sh)
docs/            plan, API contract, decision log
```

## Commands

| Command | What it does |
|---|---|
| `make build` | Build the Lambdas, `go build` the backend and type-check `infra/` |
| `make build-lambda` | Cross-compile the API and worker Lambdas (linux/arm64) into `backend/build/{api,worker}`, which CDK packages |
| `make test` | Build the Lambdas, then gofmt check, `go vet`, `go test`, and the CDK assertion tests (all offline) |
| `make test-local` | Concurrency and duplicate tests against DynamoDB Local in a throwaway Docker container (needs Docker, not Java or AWS) |
| `make deploy` | Deploy the `pokedrop-dev` stack with CDK |
| `make hotswap` | Fast CDK deploy for Lambda code changes |
| `make seed` | Write the three seed cards to the dev inventory table |
| `make token` | Print an access token for the dev test user, creating the user on first use (see below) |

All AWS targets use profile `pokedrop` and region `us-west-2`. Use `STAGE=test make deploy` for a
separate stack (`pokedrop-test`).

`make deploy` needs a terminal: CDK asks for confirmation before applying IAM and other
security-sensitive changes and refuses to continue without a TTY. Run it from an interactive shell
(in Claude Code, prefix it with `!`). Review `cd infra && npx cdk diff -c stage=dev` first.

The values the frontend needs (API URL, Cognito IDs and domain) are in the stack outputs listed in
[docs/api.md](docs/api.md#stack-outputs).

### Dev test token

`make token` signs in a Cognito test user through the dev-only app client (`ADMIN_USER_PASSWORD_AUTH`,
never created for `prod`) and prints the **access token** to stdout; everything else goes to stderr.
Put `TEST_USER_EMAIL` and `TEST_USER_PASSWORD` in `.env.local` (see `.env.example`); the password must
meet the Cognito password policy. Environment variables override the file, so a second trainer is
`TEST_USER_EMAIL=other@example.com make token`. The user is created on first use; an existing user's
password is never changed.

```sh
TOKEN=$(make -s token)
curl -H "Authorization: Bearer $TOKEN" "$API_URL/cards"
```

The API only accepts the access token. To check that it rejects an ID token (403), print one with
`cd backend && go run ./cmd/token -type id`.

### Ownership fixture

The worker writes real outcomes now, but the fixture still gives a fixed request ID to test
ownership with: `scripts/ownership-fixture.sh add` inserts one `CONFIRMED` reservation owned by the `make token` user
(marked as a fixture) and `scripts/ownership-fixture.sh remove` deletes it. With a second user's token
(`TEST_USER_EMAIL=... make token`) `GET /reservations/ffffffff-0000-4000-8000-000000000001` must return
404 while the owner gets 200. It writes data to a dev table only and refuses `prod`.

### Seeding

`make seed` is idempotent: it writes each card unconditionally, so re-running **resets stock** to the
seed values (10 / 5 / 1). It never touches the reservations table, so reseeding after real
reservations leaves stock out of step with them. Use it on dev stacks only.

## Teardown

Dev tables use `RemovalPolicy.DESTROY`, so destroying the stack deletes the tables and all their data:

```sh
cd infra && npx cdk destroy -c stage=dev --profile pokedrop
```

CDK asks for confirmation; answer `y`. In a non-interactive shell (scripts, CI, coding agents) it
refuses to run without a terminal, so add `--force` to skip the prompt.

Use `-c stage=test` for the test stack. This does not remove the shared `CDKToolkit` bootstrap stack
or its S3 bucket, and it does not affect the account's AWS Budget; those live outside this app.
Redeploy with `make deploy` and reseed with `make seed`.

The user pool is destroyed with the stack, including test users (`make token` recreates them), and a
recreated stack gets new Cognito IDs and a new hosted UI domain, so update the
[stack outputs](docs/api.md#stack-outputs) after redeploying.
