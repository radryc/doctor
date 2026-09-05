MONOFS_DIR ?= ../monofs
SERVICE ?= doctor-ingest
SERVICES ?= doctor-ingest doctor-query

.PHONY: test build vet docker-build image image-all frontend frontend-dev verify-correlation

test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./cmd/$(SERVICE)

frontend:
	cd frontend && npm ci && npm run build

frontend-dev:
	cd frontend && npm run dev

docker-build:
	@if [ ! -d "$(MONOFS_DIR)" ]; then echo "MonoFS repo not found at $(MONOFS_DIR)"; exit 1; fi
	docker buildx build --build-context monofs=$(abspath $(MONOFS_DIR)) --build-arg DOCTOR_SERVICE=$(SERVICE) -t $(SERVICE):latest .

# Build a single service image (SERVICE=doctor-ingest|doctor-query)
image: docker-build

# Build all service images
image-all:
	@for svc in $(SERVICES); do \
		$(MAKE) docker-build SERVICE=$$svc || exit 1; \
	done

verify-correlation:
	./scripts/verify-correlation.sh
