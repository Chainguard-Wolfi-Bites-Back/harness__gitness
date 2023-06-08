ifndef GOPATH
	GOPATH := $(shell go env GOPATH)
endif
ifndef GOBIN # derive value from gopath (default to first entry, similar to 'go get')
	GOBIN := $(shell go env GOPATH | sed 's/:.*//')/bin
endif
ifndef DOCKER_BUILD_OPTS
	DOCKER_BUILD_OPTS :=
endif

tools = $(addprefix $(GOBIN)/, golangci-lint goimports govulncheck protoc-gen-go protoc-gen-go-grpc gci)
deps = $(addprefix $(GOBIN)/, wire dbmate mockgen)

LDFLAGS = "-X github.com/harness/gitness/version.GitCommit=${GIT_COMMIT} -X github.com/harness/gitness/version.major=${GITNESS_VERSION_MAJOR} -X github.com/harness/gitness/version.minor=${GITNESS_VERSION_MINOR} -X github.com/harness/gitness/version.patch=${GITNESS_VERSION_PATCH}"

ifneq (,$(wildcard ./.local.env))
    include ./.local.env
    export
endif

.DEFAULT_GOAL := all

init: ## Install git hooks to perform pre-commit checks
	git config core.hooksPath .githooks
	git config commit.template .gitmessage

all: dep tools generate lint build test ## Build and run the test for gitness
	@echo "Run `make start`  to start the services"

dep: $(deps) ## Install the deps required to generate code and build gitness
	@echo "Installing dependencies"
	@go mod download

tools: $(tools) ## Install tools required for the build
	@echo "Installed tools"

mocks: $(mocks)
	@echo "Generating Test Mocks"

wire: cli/server/harness.wire_gen.go cli/server/standalone.wire_gen.go cmd/gitrpcserver/wire_gen.go

force-wire: ## Force wire code generation
	@sh ./scripts/wire/server/standalone.sh
	@sh ./scripts/wire/server/harness.sh
	@sh ./scripts/wire/gitrpcserver/wire.sh

generate: $(mocks) wire mocks/mock_client.go proto
	@echo "Generating Code"

build: generate ## Build the all-in-one gitness binary
	@echo "Building Gitness Server"
	go build -ldflags=${LDFLAGS} -o ./gitness ./cmd/gitness

harness-build: generate ## Build the all-in-one gitness binary for harness embedded mode
	@echo "Building Gitness Server for Harness"
	go build -tags=harness -ldflags=${LDFLAGS} -o ./gitness ./cmd/gitness

build-gitrpc: generate ## Build the gitrpc binary
	@echo "Building GitRPC Server"
	go build -ldflags=${LDFLAGS} -o ./gitrpcserver ./cmd/gitrpcserver

build-githook: generate ## Build the githook binary
	@echo "Building GitHook Binary"
	go build -ldflags=${LDFLAGS} -o ./githook ./cmd/githook

test: generate  ## Run the go tests
	@echo "Running tests"
	go test -v -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out

run: dep ## Run the gitness binary from source
	@go run -race -ldflags=${LDFLAGS} .

clean-db: ## delete all data from local database
	psql postgresql://gitness:gitness@localhost:5432/gitness -f scripts/db/cleanup.sql

populate-db: ## inject sample data into local database
	psql postgresql://gitness:gitness@localhost:5432/gitness -f scripts/db/sample_data.sql

update-tools: delete-tools $(tools) ## Update the tools by deleting and re-installing

delete-tools: ## Delete the tools
	@rm $(tools) || true

#########################################
# Docker environment commands
# The following targets relate to running gitness and its dependent services
#########################################
start: ## Run all dependent services and start the gitness server locally - the service will listen on :3000 by default
	docker-compose -f ./docker/docker-compose.yml up ${DOCKER_BUILD_OPTS} --remove-orphans

stop: ## Stop all services
	docker-compose -f ./docker/docker-compose.yml down --remove-orphans

dev: ## Run local dev environment this starts the services which gitness depends on
	docker-compose -f ./docker/docker-compose.yml up ${DOCKER_BUILD_OPTS} --remove-orphans db redis

test-env: stop ## Run test environment - this runs all services and the gitness in test mode.
	docker-compose -f ./docker/docker-compose.yml -f ./docker/docker-compose.test.yml up -d ${DOCKER_BUILD_OPTS} --remove-orphans

image: ## Build the gitness docker image
	@echo "Building Gitness Image"
	@docker build \
			--build-arg GITNESS_VERSION=latest \
			--build-arg GIT_COMMIT=${GIT_COMMIT} \
			--build-arg GITHUB_ACCESS_TOKEN=${GITHUB_ACCESS_TOKEN} \
			--platform linux/amd64 \
			 -t gitness:latest \
			 -f ./docker/Dockerfile .

e2e: generate test-env ## Run e2e tests
	chmod +x wait-for-gitness.sh && ./wait-for-gitness.sh
	go test -p 1 -v -coverprofile=e2e_cov.out ./tests/... -env=".env.local"


###########################################
# Code Formatting and linting
###########################################

format: tools # Format go code and error if any changes are made
	@echo "Formating ..."
	@goimports -w .
	@gci write --custom-order -s standard -s "prefix(github.com/harness/gitness)" -s default -s blank -s dot .
	@echo "Formatting complete"

sec:
	@echo "Vulnerability detection $(1)"
	@govulncheck ./...

lint: tools generate # lint the golang code
	@echo "Linting $(1)"
	@golangci-lint run --timeout=3m --verbose

###########################################
# Code Generation
#
# Some code generation can be slow, so we only run it if
# the source file has changed.
###########################################
cli/server/harness.wire_gen.go: cli/server/harness.wire.go
	@sh ./scripts/wire/server/harness.sh

cli/server/standalone.wire_gen.go: cli/server/standalone.wire.go
	@sh ./scripts/wire/server/standalone.sh

cmd/gitrpcserver/wire_gen.go: cmd/gitrpcserver/wire.go
	@sh ./scripts/wire/gitrpcserver/wire.sh

mocks/mock_client.go: internal/store/database.go client/client.go
	go generate mocks/mock.go

proto:
	@protoc --proto_path=./gitrpc/proto \
			--go_out=./gitrpc/rpc \
			--go_opt=paths=source_relative \
			--go-grpc_out=./gitrpc/rpc \
			--go-grpc_opt=paths=source_relative \
			./gitrpc/proto/*.proto

harness-proto:
	@protoc --proto_path=./harness/proto \
			--go_out=./harness/rpc \
			--go_opt=paths=source_relative \
			--go-grpc_out=./harness/rpc \
			--go-grpc_opt=paths=source_relative \
			./harness/proto/*.proto
###########################################
# Install Tools and deps
#
# These targets specify the full path to where the tool is installed
# If the tool already exists it wont be re-installed.
###########################################

# Install golangci-lint
$(GOBIN)/golangci-lint:
	@echo "🔘 Installing golangci-lint... (`date '+%H:%M:%S'`)"
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(GOBIN)

# Install goimports to format code
$(GOBIN)/goimports:
	@echo "🔘 Installing goimports ... (`date '+%H:%M:%S'`)"
	@go install golang.org/x/tools/cmd/goimports

# Install wire to generate dependency injection
$(GOBIN)/wire:
	go install github.com/google/wire/cmd/wire@latest

# Install dbmate to perform db migrations
$(GOBIN)/dbmate:
	go install github.com/amacneil/dbmate@v1.15.0

# Install mockgen to generate mocks
$(GOBIN)/mockgen:
	go install github.com/golang/mock/mockgen@latest

$(GOBIN)/govulncheck:
	go install golang.org/x/vuln/cmd/govulncheck@latest

$(GOBIN)/protoc-gen-go:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.28

$(GOBIN)/protoc-gen-go-grpc:
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.2

$(GOBIN)/gci:
	go install github.com/daixiang0/gci@latest

help: ## show help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m\033[0m\n"} /^[$$()% 0-9a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: delete-tools update-tools help format lint