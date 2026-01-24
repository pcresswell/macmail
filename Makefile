.PHONY: build install clean test coverage

BINARY := macmail
VERSION_FILE := VERSION
PACKAGE := ./cmd/macmail

# Read version from file
VERSION := $(shell cat $(VERSION_FILE) | tr -d '[:space:]')

# Increment patch version (used by build/install targets)
MAJOR := $(word 1,$(subst ., ,$(VERSION)))
MINOR := $(word 2,$(subst ., ,$(VERSION)))
PATCH := $(word 3,$(subst ., ,$(VERSION)))
NEW_PATCH := $(shell echo $$(($(PATCH) + 1)))
NEW_VERSION := $(MAJOR).$(MINOR).$(NEW_PATCH)

LDFLAGS := -ldflags "-X main.version=$(NEW_VERSION)"

test:
	@echo "Running tests..."
	@go test ./cmd/macmail -cover
	@echo ""

build: test
	@echo "$(NEW_VERSION)" > $(VERSION_FILE)
	@echo "Building $(BINARY) v$(NEW_VERSION)..."
	@go build $(LDFLAGS) -o $(BINARY) $(PACKAGE)
	@echo "Built $(BINARY) v$(NEW_VERSION)"

install: test
	@echo "$(NEW_VERSION)" > $(VERSION_FILE)
	@echo "Installing $(BINARY) v$(NEW_VERSION)..."
	@go install $(LDFLAGS) $(PACKAGE)
	@echo "Installed $(BINARY) v$(NEW_VERSION)"

clean:
	rm -f $(BINARY) coverage.out

coverage:
	@go test ./cmd/macmail -coverprofile=coverage.out
	@go tool cover -html=coverage.out

# Build without incrementing version (for CI/testing)
build-current: test
	@go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PACKAGE)

version:
	@echo $(VERSION)
