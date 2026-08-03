VERSION ?= 1.0.0
OUT ?= dist
PLUGIN = codex-carpool
ARTIFACT = $(PLUGIN)_$(VERSION)

.PHONY: test test-race build package clean

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

build:
	mkdir -p $(OUT)
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w -X main.pluginVersion=$(VERSION)" -o $(OUT)/$(ARTIFACT).so ./cmd/$(PLUGIN)

package: build
	mkdir -p $(OUT)/package
	cp $(OUT)/$(ARTIFACT).so $(OUT)/package/$(ARTIFACT).so
	cp README.md $(OUT)/package/README.md
	printf '%s\n' '$(VERSION)' > $(OUT)/package/VERSION
	cd $(OUT)/package && zip -9 ../$(PLUGIN)_$(VERSION)_linux_amd64.zip $(ARTIFACT).so README.md VERSION

clean:
	rm -rf $(OUT)
