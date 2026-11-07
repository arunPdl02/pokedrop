# PokéDrop
Pokémon card reservation app. React+TS (Vite), Go Lambdas, Cognito, API Gateway HTTP API,
SQS, DynamoDB, S3+CloudFront, CDK (TypeScript), GitHub Actions.
Plan: docs/PLAN.md · API contract: docs/api.md

## Workflow
- Work only on the current phase in docs/PLAN.md. Propose a plan before writing code.
- A phase is done when: tests pass, the milestone is verified against the deployed dev stack,
  PLAN.md boxes are ticked, docs/decisions.md has an entry, and work is committed
  using the phase's commit messages.
- When a phase is complete, end with a summary and remind me to run:
  `git tag phase-N && git push && git push --tags`
- Before every deploy, run `npx cdk diff` as its own command, show me the IAM and resource changes, and wait for my approval. Never chain deploy with other commands.

## Invariants (never violate)
- Stock never goes negative.
- A repeated request ID never consumes inventory twice.
- Trainers can only read their own reservations.
- POST /reservations returns 202 = queued, not reserved.

## Code
- Business logic in backend/internal/; Lambda handlers stay thin.
- No secrets in the repo; local secrets live in .env.local (gitignored).

## AWS
- Profile pokedrop, region us-west-2. Create and change resources ONLY through CDK,
  never with the console or ad-hoc CLI writes. Read-only CLI calls for debugging are fine.
- Tag stacks project=pokedrop, 1-week log retention, dev tables use RemovalPolicy.DESTROY.
- On ExpiredToken errors, stop and ask me to run `aws login`.

## Commands
make test · make deploy · make hotswap · make token

## Git
- Conventional commits: `type: imperative summary` (≤72 chars), types feat/fix/test/docs/chore/ci/perf/refactor.
  Use the target commit messages in docs/PLAN.md when they fit. Body (optional) explains why, not what.
- No Co-Authored-By or "Generated with" lines.
- One logical change per commit. Every commit must build and pass `make test`; never commit failing code.
- Run `git status` and stage specific paths. Never `git add -A` or `git add .` without reviewing the status first.
- Never commit: .env*, secrets, tokens, cdk.out/, node_modules/, build/, dist/, coverage/. Always commit lockfiles.
- Never: force push, rewrite or amend pushed commits, `git reset --hard`, `git clean`, delete branches,
  or change git config. Ask me if one of these seems necessary.
- Don't push unless I ask. Don't create tags; I tag phases myself.
- In a worktree, commit only to that worktree's branch and only to files in its scope.
