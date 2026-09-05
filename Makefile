VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test lint e2e clean

build:
	go build $(LDFLAGS) -o bin/twigd ./cmd/twigd
	go build $(LDFLAGS) -o bin/twig ./cmd/twig

test:
	go vet ./...
	go test -race ./...

lint:
	golangci-lint run

# E2E を素の Ubuntu ホスト(VM/EC2)上で直接実行する場合:
#   sudo ./e2e/e2e.sh bin/twigd bin/twig
e2e:
	@echo "Ubuntu ホスト上で: sudo ./e2e/e2e.sh <twigd> <twig>"
	@echo "macOS からは: make e2e-local (Lima VM を使う)"
	@exit 1

# macOS から Lima VM で E2E を回す。要: brew install lima
LIMA_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
e2e-local:
	GOOS=linux GOARCH=$(LIMA_ARCH) go build $(LDFLAGS) -o bin/linux/twigd ./cmd/twigd
	GOOS=linux GOARCH=$(LIMA_ARCH) go build $(LDFLAGS) -o bin/linux/twig ./cmd/twig
	@limactl list --format '{{.Name}}' 2>/dev/null | grep -qx twig-e2e || \
		limactl start --name=twig-e2e ./e2e/lima.yaml --yes
	limactl copy bin/linux/twigd bin/linux/twig e2e/e2e.sh twig-e2e:/tmp/
	limactl copy action/entrypoint.sh twig-e2e:/tmp/action-entrypoint.sh
	limactl shell twig-e2e sudo bash /tmp/e2e.sh /tmp/twigd /tmp/twig

# PostgreSQL エンジンの E2E (Lima VM)
e2e-postgres-local:
	GOOS=linux GOARCH=$(LIMA_ARCH) go build $(LDFLAGS) -o bin/linux/twigd ./cmd/twigd
	GOOS=linux GOARCH=$(LIMA_ARCH) go build $(LDFLAGS) -o bin/linux/twig ./cmd/twig
	@limactl list --format '{{.Name}}' 2>/dev/null | grep -qx twig-e2e || \
		limactl start --name=twig-e2e ./e2e/lima.yaml --yes
	limactl copy bin/linux/twigd bin/linux/twig e2e/postgres/e2e.sh deploy/systemd/postgres-twig@.service twig-e2e:/tmp/
	limactl shell twig-e2e sudo bash -c 'mkdir -p /tmp/pg-e2e && cp /tmp/e2e.sh /tmp/postgres-twig@.service /tmp/pg-e2e/ && bash /tmp/pg-e2e/e2e.sh /tmp/twigd /tmp/twig'

e2e-local-clean:
	limactl delete -f twig-e2e 2>/dev/null || true

clean:
	rm -rf bin/
