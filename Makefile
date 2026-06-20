.PHONY: build run test tidy vet clean

build:
	go build -o bin/strand ./cmd/strand

run:
	go run ./cmd/strand

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
