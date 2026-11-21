.PHONY: prepare build test check up

prepare:
	sh tools/prepare.sh

build: prepare
	mkdir -p dist
	go build -mod=readonly -trimpath -o dist/arctex-emulator ./cmd/emulator

test: prepare
	go test -mod=readonly -race ./...

check: test
	go vet -mod=readonly ./...

up:
	docker compose up --build -d --wait
