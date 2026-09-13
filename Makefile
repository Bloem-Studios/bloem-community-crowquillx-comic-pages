.PHONY: test build

test:
	go test -count=1 ./...
	go vet ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/plugin .
