.PHONY: run demo test race fmt vet build docker

# Run against the real PayPal sandbox (needs a filled-in .env).
run:
	set -a; . ./.env; set +a; go run ./cmd/advance

# Run fully offline against the bundled PayPal mock. No credentials needed.
demo:
	./scripts/demo.sh

test:
	go test ./...

race:
	go test -race -count=1 ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

build:
	go build -o bin/advance ./cmd/advance && go build -o bin/mockpaypal ./cmd/mockpaypal

docker:
	docker build -t advance .
