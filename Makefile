# Artçı geliştirme komutları.  `make help` ile listeleyin.
SHELL := /bin/sh

GO      ?= go
COMPOSE ?= docker compose
DEV     := $(COMPOSE) -f deploy/docker-compose.dev.yml
PROD    := $(COMPOSE) --env-file deploy/.env -f deploy/docker-compose.yml
BIN     := bin
CATALOG ?= /tmp/sindirgi.jsonl
SPEED   ?= 10

# Yerel varsayılanlar (dev compose ile uyumlu); ortamdan ezilebilir.
export ARTCI_POSTGRES_DSN ?= postgres://artci:artci@localhost:5432/artci?sslmode=disable
export ARTCI_REDIS_ADDR   ?= localhost:6379

.DEFAULT_GOAL := help
.PHONY: help dev-up dev-down build test test-integration redteam lint \
        run-ingest-afad run-ingest-kandilli run-ingest-emsc run-merger run-notifier \
        run-summarizer run-api replay-gen replay replay-real docker-build up down

help: ## Bu yardım
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-22s %s\n", $$1, $$2}'

# --- Yerel altyapı ---
dev-up: ## PostGIS + Redis'i başlat
	$(DEV) up -d --wait

dev-down: ## Yerel altyapıyı durdur (veri korunur)
	$(DEV) down

# --- Derleme ve test (CI ile aynı adımlar) ---
build: ## Tüm komutları bin/ altına derle
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/ ./cmd/...

test: ## Birim testleri (DB gerektirmez; entegrasyon testleri atlanır)
	$(GO) test ./...

test-integration: ## PostGIS entegrasyon testleri dahil, -race ile (önce: make dev-up)
	ARTCI_TEST_POSTGRES_DSN='$(ARTCI_POSTGRES_DSN)' $(GO) test ./... -race -count=1

redteam: ## Özet doğrulayıcısının red-team seti
	$(GO) test ./internal/summary -run 'RedTeam|TemplateAlwaysValid' -count=1 -v

lint: ## gofmt + go vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt gerekli:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

# --- Servisleri yerelde çalıştırma (her biri ayrı terminalde) ---
# Aynı makinede çakışmasın diye her süreç farklı metrik portu kullanır.
run-ingest-afad: ## AFAD toplayıcısı (metrik :9101)
	ARTCI_METRICS_ADDR=:9101 $(GO) run ./cmd/ingest -source=afad

run-ingest-kandilli: ## Kandilli toplayıcısı (metrik :9111)
	ARTCI_METRICS_ADDR=:9111 $(GO) run ./cmd/ingest -source=kandilli

run-ingest-emsc: ## EMSC toplayıcısı (metrik :9121)
	ARTCI_METRICS_ADDR=:9121 $(GO) run ./cmd/ingest -source=emsc

run-merger: ## Tekilleştirici (metrik :9102)
	ARTCI_METRICS_ADDR=:9102 $(GO) run ./cmd/merger

run-notifier: ## Telegram bildirici; ARTCI_TELEGRAM_TOKEN yoksa dry-run (metrik :9103)
	ARTCI_METRICS_ADDR=:9103 $(GO) run ./cmd/notifier

run-summarizer: ## Özetleyici; ARTCI_LLM_API_KEY yoksa yalnızca şablon (metrik :9104)
	ARTCI_METRICS_ADDR=:9104 $(GO) run ./cmd/summarizer

run-api: ## REST + WebSocket + harita, http://localhost:8080 (metrik :9105)
	ARTCI_HTTP_ADDR=:8080 ARTCI_METRICS_ADDR=:9105 $(GO) run ./cmd/api

# --- Ölçüm ---
replay-gen: ## Sentetik artçı dizisi üret (CATALOG)
	$(GO) run ./cmd/replay gen -out $(CATALOG)

replay: ## Kataloğu oynatıp doğruluk ve gecikmeyi ölç (merger çalışıyor olmalı)
	$(GO) run ./cmd/replay run -in $(CATALOG) -speed $(SPEED) -cleanup

replay-real: ## Gerçek Sındırgı 2025 dizisini (ISC) beklemeden oynat (merger çalışıyor olmalı)
	$(GO) run ./cmd/replay run -in testdata/replay/sindirgi-isc-2025-08.jsonl.gz -speed 0 -cleanup

# --- Üretim ---
docker-build: ## İmajı derle (artci:latest)
	docker build -t artci:latest .

up: ## Üretim yığınını başlat (deploy/.env gerekir)
	$(PROD) up -d --build

down: ## Üretim yığınını durdur
	$(PROD) down
