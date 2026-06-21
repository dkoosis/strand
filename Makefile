.PHONY: build run test race lint vet tidy check clean

build:
	go build -o bin/strand ./cmd/strand

run:
	go run ./cmd/strand

test:
	go test ./...

# race runs the suite under the race detector.
race:
	go test -race -count=1 ./...

# lint runs the strict golangci-lint set (.golangci.yml).
lint:
	golangci-lint run ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# check is the full local gate: vet + strict lint + race.
check: vet lint race

clean:
	rm -rf bin
