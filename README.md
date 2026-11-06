# PokéDrop

Pokémon card reservation app: trainers compete to reserve limited cards during simulated drops.
A Go backend on AWS processes reservations asynchronously and protects inventory under concurrent
requests and retries. See [docs/PLAN.md](docs/PLAN.md) for the plan and [docs/api.md](docs/api.md)
for the API contract.

Status: Phase 1 (repository, infrastructure and seed data). No API or UI yet.

## Prerequisites

- Go (see `backend/go.mod`), Node.js 22+, GNU Make
- AWS CLI v2 with a profile named `pokedrop` (region `us-west-2`)
- The account/region bootstrapped for CDK (`CDKToolkit` stack in us-west-2)
- Optional: copy `.env.example` to `.env.local` (gitignored) for local secrets. Nothing in Phase 1
  needs it.

Sign in before deploying or seeding. If a command fails with `ExpiredToken`, run:

```sh
aws login --profile pokedrop
```

## Layout

```text
frontend/        placeholder until Phase 4
backend/         Go module: cmd/ (thin Lambda entry points, seed), internal/ (rules, storage)
infra/           CDK app (TypeScript)
tests/load/      load tests (Phase 6)
scripts/         helper scripts
docs/            plan, API contract, decision log
```

## Commands

| Command | What it does |
|---|---|
| `make build` | `go build` the backend and type-check `infra/` |
| `make test` | gofmt check, `go vet`, `go test`, and the CDK assertion tests (all offline) |
| `make deploy` | Deploy the `pokedrop-dev` stack with CDK |
| `make hotswap` | Fast CDK deploy for Lambda changes (nothing to hotswap until Phase 2) |
| `make seed` | Write the three seed cards to the dev inventory table |
| `make token` | Print an access token for the dev test user, creating the user on first use (see below) |

All AWS targets use profile `pokedrop` and region `us-west-2`. Use `STAGE=test make deploy` for a
separate stack (`pokedrop-test`).

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
