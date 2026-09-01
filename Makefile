.PHONY: build check test vet

build:
	go build -o jmapsync ./cmd/jmapsync

test:
	go test ./...

vet:
	go vet ./...

check: test vet

