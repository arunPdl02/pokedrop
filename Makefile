export AWS_PROFILE ?= pokedrop
export AWS_REGION ?= us-west-2
STAGE ?= dev
CDK = cd infra && npx cdk

.PHONY: build test deploy hotswap seed token

build:
	cd backend && go build ./...
	cd infra && npm run --silent build

test:
	@test -z "$$(gofmt -l backend)" || { echo "gofmt needed:"; gofmt -l backend; exit 1; }
	cd backend && go vet ./...
	cd backend && go test ./...
	cd infra && npm test --silent

# Full deploy through CDK (the only way resources are created or changed).
# CDK prompts before applying IAM/security-group changes.
deploy: build
	$(CDK) deploy --all -c stage=$(STAGE)

# Fast path for Lambda code/config changes. Nothing to hotswap until Lambdas exist (Phase 2);
# CDK falls back to a full deployment for anything it cannot hotswap.
hotswap: build
	$(CDK) deploy --all --hotswap -c stage=$(STAGE)

# Reset the dev inventory to the three seed cards (idempotent; resets stock, leaves
# reservations untouched). Needs a deployed stack and valid AWS credentials.
seed:
	cd backend && go run ./cmd/seed -stage $(STAGE)

# Print an access token for the dev test user (created on first use) on stdout; messages go to
# stderr, so TOKEN=$$(make -s token) works. Reads TEST_USER_EMAIL / TEST_USER_PASSWORD from the
# environment or .env.local. Uses the dev-only app client, so it refuses the prod stage.
# Use TEST_USER_EMAIL=other@example.com make token for a second trainer.
token:
	@cd backend && go run ./cmd/token -stage $(STAGE)
