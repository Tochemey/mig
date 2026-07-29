.PHONY: build vet lint test cover cover-html clean

MODULE   := github.com/tochemey/mig
COVERDIR := .cover
PROFILE  := $(COVERDIR)/coverage.out

build:
	go build ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test:
	go test ./... -race -count=1 -timeout 20m

# Coverage has to account for two sources. The parent test binaries report
# through -coverprofile as usual; the migrator and the matrix fixtures only ever
# run as child processes, and those are built with -cover and report into
# COVERDIR. Counting only the first would show the executor — the code the kill
# matrices exercise hardest — as untested.
cover:
	rm -rf $(COVERDIR)
	mkdir -p $(COVERDIR)/children
	MIG_COVERDIR=$(abspath $(COVERDIR)/children) \
		go test ./... -count=1 -timeout 20m \
			-coverpkg=./... -coverprofile=$(COVERDIR)/parent.out
	go tool covdata textfmt -i=$(COVERDIR)/children -o=$(COVERDIR)/children.out
	head -1 $(COVERDIR)/parent.out > $(PROFILE)
	tail -n +2 -q $(COVERDIR)/parent.out $(COVERDIR)/children.out >> $(PROFILE)
	go tool cover -func=$(PROFILE) | tail -1
	@echo
	@echo "Below 100%:"
	@go tool cover -func=$(PROFILE) | grep -v '100.0%$$' | grep -v '^total:' || echo "  none"

cover-html: cover
	go tool cover -html=$(PROFILE)

clean:
	rm -rf $(COVERDIR)
