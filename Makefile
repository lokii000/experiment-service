.PHONY: run run-db test race vet fmt
run:
	go run ./cmd/api
run-db:
	go run -tags pgx ./cmd/api

test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
fmt:
	gofmt -w cmd internal
