export AWS_PROFILE ?= pokedrop
export AWS_REGION ?= us-west-2
STAGE ?= dev
CDK = cd infra && npx cdk

.PHONY: frontend-config install build build-frontend build-lambda lint test test-local test-local-run test-integration test-integration-failure load drain-verify deploy hotswap destroy fail-on fail-off fail-status seed token

DYNAMODB_LOCAL_IMAGE = amazon/dynamodb-local:3.3.2
DYNAMODB_LOCAL_PORT ?= 8000
DYNAMODB_LOCAL_NAME = pokedrop-dynamodb-local

# Install the exact locked npm dependencies (what CI uses). Go modules are fetched on demand.
install:
	cd infra && npm ci
	cd frontend && npm ci

build: build-lambda build-frontend
	cd backend && go build ./...
	cd infra && npm run --silent build

# Type-check and bundle the SPA into frontend/dist.
build-frontend:
	cd frontend && npm run --silent build

# Cross-compile the Lambdas for provided.al2023 on arm64. CDK packages backend/build/<name> as the
# function code, so they must exist before `cdk synth` (and therefore before the infra tests).
# -trimpath, -buildvcs=false and -s -w keep the binaries reproducible so an unchanged build is not
# redeployed: without -buildvcs=false, Go stamps the git commit into each binary, so every commit
# would change both Lambdas' hashes.
LAMBDA_BUILD = GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -buildvcs=false -ldflags="-s -w"

build-lambda:
	cd backend && $(LAMBDA_BUILD) -o build/api/bootstrap ./cmd/api
	cd backend && $(LAMBDA_BUILD) -o build/worker/bootstrap ./cmd/worker

# Formatting and static checks, also run by CI as its own step.
lint:
	@test -z "$$(gofmt -l backend)" || { echo "gofmt needed:"; gofmt -l backend; exit 1; }
	cd backend && go vet ./...

# The infra tests synthesize the prod stack, which packages frontend/dist, so the SPA is built first.
test: lint build-lambda build-frontend
	cd backend && go test ./...
	cd infra && npm test --silent
	cd frontend && npm test --silent

# Concurrency and duplicate tests against DynamoDB Local (needs Docker, not Java or AWS). Starts a
# throwaway in-memory container, runs the `dynamodblocal`-tagged tests, and always removes it.
test-local:
	docker run -d --rm --name $(DYNAMODB_LOCAL_NAME) -p 127.0.0.1:$(DYNAMODB_LOCAL_PORT):8000 $(DYNAMODB_LOCAL_IMAGE) -jar DynamoDBLocal.jar -inMemory >/dev/null
	$(MAKE) --no-print-directory test-local-run; status=$$?; docker stop $(DYNAMODB_LOCAL_NAME) >/dev/null; exit $$status

# The same tests against a DynamoDB Local that is already running at DYNAMODB_LOCAL_ENDPOINT. CI
# uses this with a service container; `make test-local` starts its own container and calls it.
DYNAMODB_LOCAL_ENDPOINT ?= http://localhost:$(DYNAMODB_LOCAL_PORT)

test-local-run:
	cd backend && DYNAMODB_LOCAL_ENDPOINT=$(DYNAMODB_LOCAL_ENDPOINT) go test -count=1 -tags dynamodblocal -run '^TestLocal' -v ./internal/storage/

# Acceptance tests against the deployed pokedrop-test stack (docs/PLAN.md, Phase 6). They reset card
# stock, so they only run with STAGE=test. Tokens for two trainers come from `make token`: trainer
# A is TEST_USER_EMAIL, trainer B is TRAINER_B_EMAIL (created on first use, same password).
#   make test-integration STAGE=test          rows 1, 2 and 5 (failure injection must be off)
#   make test-integration-failure STAGE=test  row 3 (needs make fail-on STAGE=test MODE=once)
TRAINER_B_EMAIL ?= trainer-b@example.test

define run_integration
	@test "$(STAGE)" = test || { echo "integration tests reset stock and only run with STAGE=test (STAGE=$(STAGE))" >&2; exit 1; }
	@a="$$($(MAKE) --no-print-directory -s token STAGE=$(STAGE))" && \
		b="$$(TEST_USER_EMAIL=$(TRAINER_B_EMAIL) $(MAKE) --no-print-directory -s token STAGE=$(STAGE))" && \
		cd backend && STAGE=$(STAGE) TOKEN_A="$$a" TOKEN_B="$$b" \
		go test -count=1 -tags integration -timeout 20m -v -run '$(1)' ./integration/
endef

test-integration:
	$(call run_integration,^(TestOversell|TestDuplicates|TestDuplicateOfSoldOutStaysSoldOut|TestCrossTrainerRead)$$)

test-integration-failure:
	$(call run_integration,^TestFailureAfterCommit$$)

# Load test the deployed pokedrop-test stack (docs/benchmark.md has the full procedure). One run:
#   make seed STAGE=test STOCK=100
#   make load STAGE=test RUN=full REQUESTS=200 CONCURRENCY=4
#   make drain-verify STAGE=test RUN=full STOCK=100
# `load` runs the preflight (which refuses a concurrency the account's Lambda limit cannot take),
# then k6, which writes one line per request to tests/load/results/<RUN>/submits.jsonl. `drain-verify`
# waits for the queue to drain, checks the invariants and prints the benchmark numbers. Test stage only.
RUN ?=
REQUESTS ?= 20
CONCURRENCY ?= 2
CARD_ID ?= card-001
LOAD_RESULTS = $(CURDIR)/tests/load/results

load:
	@test "$(STAGE)" = test || { echo "load tests only run with STAGE=test (STAGE=$(STAGE))" >&2; exit 1; }
	@test -n "$(RUN)" || { echo "set RUN=<name>, for example: make load STAGE=test RUN=small" >&2; exit 1; }
	cd backend && go run ./cmd/drainverify preflight -stage $(STAGE) -run $(RUN) -requests $(REQUESTS) \
		-concurrency $(CONCURRENCY) -card $(CARD_ID) -results $(LOAD_RESULTS)
	@api="$$(aws cloudformation describe-stacks --stack-name pokedrop-$(STAGE) --output text \
			--query "Stacks[0].Outputs[?OutputKey=='ApiUrl'].OutputValue | [0]")" && \
		token="$$($(MAKE) --no-print-directory -s token STAGE=$(STAGE))" && \
		API_URL="$$api" TOKEN="$$token" CARD_ID="$(CARD_ID)" REQUESTS="$(REQUESTS)" CONCURRENCY="$(CONCURRENCY)" \
		k6 run -q --console-output "$(LOAD_RESULTS)/$(RUN)/submits.jsonl" tests/load/burst.js

drain-verify:
	@test "$(STAGE)" = test || { echo "load tests only run with STAGE=test (STAGE=$(STAGE))" >&2; exit 1; }
	@test -n "$(RUN)" || { echo "set RUN=<name> of a finished make load" >&2; exit 1; }
	@test -n "$(STOCK)" || { echo "set STOCK=<the stock the card was seeded with before the run>" >&2; exit 1; }
	cd backend && go run ./cmd/drainverify verify -stage $(STAGE) -run $(RUN) -stock $(STOCK) \
		-results $(LOAD_RESULTS) -repo $(CURDIR)

# Full deploy through CDK (the only way resources are created or changed).
# CDK prompts before applying IAM/security-group changes.
deploy: build
	$(CDK) deploy --all -c stage=$(STAGE)

# Fast path for Lambda code changes. --hotswap updates the Lambdas directly and ignores changes it
# cannot hotswap (it does not fall back to a full deployment), and CloudFormation's template is
# left as it was. Use `make deploy` for anything else.
hotswap: build
	$(CDK) deploy --all --hotswap -c stage=$(STAGE)

# Tear down the isolated test stack (Phase 6). Test only: it will not touch dev or prod. CDK asks
# for confirmation, so run it from an interactive terminal.
destroy:
	@test "$(STAGE)" = test || { echo "destroy is test-only (STAGE=$(STAGE)); use the README teardown steps for other stages" >&2; exit 1; }
	$(CDK) destroy -c stage=$(STAGE)

# Dev and test worker failure injection (docs/runbook.md): records fail after their outcome is stored.
# MODE=once fails only the delivery that stored the outcome (the retry then succeeds); MODE=always
# fails every delivery, so the message reaches the dead letter queue. These are full deploys, not
# hotswaps (see `hotswap` above), so `fail-off` always has a change to apply. Any plain
# `make deploy` also turns the flag off. CDK refuses the flag outside dev and test.
MODE ?= once

fail-on: build
	@case "$(STAGE)" in dev | test) ;; *) echo "fail-on is dev/test-only (STAGE=$(STAGE))" >&2; exit 1 ;; esac
	@case "$(MODE)" in once | always) ;; *) echo "MODE must be once or always" >&2; exit 1 ;; esac
	$(CDK) deploy --all -c stage=$(STAGE) -c failAfterCommit=$(MODE)

fail-off: build
	@case "$(STAGE)" in dev | test) ;; *) echo "fail-off is dev/test-only (STAGE=$(STAGE))" >&2; exit 1 ;; esac
	$(CDK) deploy --all -c stage=$(STAGE) -c failAfterCommit=off

# Print the deployed worker's failure injection mode (read-only).
fail-status:
	@aws lambda get-function-configuration --output text \
		--function-name "$$(aws cloudformation describe-stacks --stack-name pokedrop-$(STAGE) --output text \
			--query "Stacks[0].Outputs[?OutputKey=='WorkerFunctionName'].OutputValue | [0]")" \
		--query "Environment.Variables.FAIL_AFTER_COMMIT || 'off'"

# Reset the inventory to the three seed cards (idempotent; resets stock, leaves reservations
# untouched). STOCK=100 sets every card to 100 instead of the cards.json values (load tests).
# Needs a deployed stack and valid AWS credentials.
STOCK ?=

seed:
	cd backend && go run ./cmd/seed -stage $(STAGE) $(if $(STOCK),-stock $(STOCK))

# Print an access token for the dev test user (created on first use) on stdout; messages go to
# stderr, so TOKEN=$$(make -s token) works. Reads TEST_USER_EMAIL / TEST_USER_PASSWORD from the
# environment or .env.local. Uses the dev-only app client, so it refuses the prod stage.
# Use TEST_USER_EMAIL=other@example.com make token for a second trainer.
token:
	@cd backend && go run ./cmd/token -stage $(STAGE)

# Write frontend/public/config.json (gitignored) from the stack's outputs, so `npm run dev` talks to
# that stage. A deployed site gets its own config.json from CDK instead, so prod is refused here
# (it does not register http://localhost:5173 as a sign-in URL anyway). Read-only AWS calls.
frontend-config:
	@test "$(STAGE)" != prod || { echo "prod's config.json is written by the deploy, not by this target" >&2; exit 1; }
	@out() { aws cloudformation describe-stacks --stack-name pokedrop-$(STAGE) --output text \
			--query "Stacks[0].Outputs[?OutputKey=='$$1'].OutputValue | [0]"; }; \
		api="$$(out ApiUrl)"; client="$$(out UserPoolClientId)"; domain="$$(out HostedUiDomain)"; \
		for v in "$$api" "$$client" "$$domain"; do \
			case "$$v" in ""|None) echo "stack output missing: is pokedrop-$(STAGE) deployed?" >&2; exit 1 ;; esac; \
		done; \
		mkdir -p frontend/public && \
		printf '{\n  "apiUrl": "%s",\n  "clientId": "%s",\n  "hostedUiDomain": "%s"\n}\n' "$$api" "$$client" "$$domain" \
			> frontend/public/config.json && \
		echo "wrote frontend/public/config.json for pokedrop-$(STAGE)"
