.PHONY: build check test vet

build:
	go build -o jmap2gmail ./cmd/jmap2gmail

test:
	go test ./...

vet:
	go vet ./...

check: test vet

