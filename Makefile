.PHONY: build test

build:
	cd backend && go build ./...
	cd infra && npm run --silent build

test:
	@test -z "$$(gofmt -l backend)" || { echo "gofmt needed:"; gofmt -l backend; exit 1; }
	cd backend && go vet ./...
	cd backend && go test ./...
	cd infra && npm test --silent
