export AWS_PROFILE ?= pokedrop
export AWS_REGION ?= us-west-2
STAGE ?= dev
CDK = cd infra && npx cdk

.PHONY: build build-lambda test test-local deploy hotswap seed token

DYNAMODB_LOCAL_IMAGE = amazon/dynamodb-local:3.3.2
DYNAMODB_LOCAL_PORT ?= 8000
DYNAMODB_LOCAL_NAME = pokedrop-dynamodb-local

build: build-lambda
	cd backend && go build ./...
	cd infra && npm run --silent build

# Cross-compile the Lambdas for provided.al2023 on arm64. CDK packages backend/build/<name> as the
# function code, so they must exist before `cdk synth` (and therefore before the infra tests).
# -trimpath and -s -w keep the binaries reproducible so an unchanged build is not redeployed.
LAMBDA_BUILD = GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags="-s -w"

build-lambda:
	cd backend && $(LAMBDA_BUILD) -o build/api/bootstrap ./cmd/api
	cd backend && $(LAMBDA_BUILD) -o build/worker/bootstrap ./cmd/worker

test: build-lambda
	@test -z "$$(gofmt -l backend)" || { echo "gofmt needed:"; gofmt -l backend; exit 1; }
	cd backend && go vet ./...
	cd backend && go test ./...
	cd infra && npm test --silent

# Concurrency and duplicate tests against DynamoDB Local (needs Docker, not Java or AWS). Starts a
# throwaway in-memory container, runs the `dynamodblocal`-tagged tests, and always removes it.
test-local:
	docker run -d --rm --name $(DYNAMODB_LOCAL_NAME) -p 127.0.0.1:$(DYNAMODB_LOCAL_PORT):8000 $(DYNAMODB_LOCAL_IMAGE) -jar DynamoDBLocal.jar -inMemory >/dev/null
	cd backend && DYNAMODB_LOCAL_ENDPOINT=http://localhost:$(DYNAMODB_LOCAL_PORT) go test -count=1 -tags dynamodblocal -run '^TestLocal' -v ./internal/storage/; \
		status=$$?; docker stop $(DYNAMODB_LOCAL_NAME) >/dev/null; exit $$status

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
