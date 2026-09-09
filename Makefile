.PHONY: build test check acceptance clean
build:
	./scripts/build.sh
test:
	go test -race ./...
	cd chrome-extension && npm test
check:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"
	@for f in scripts/*.sh; do bash -n "$$f"; done
acceptance:
	go run ./scripts/acceptance.go ./bin/macbridge
clean:
	rm -rf bin "MacBridge Go.app"
