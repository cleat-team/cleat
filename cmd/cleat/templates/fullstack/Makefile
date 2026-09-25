.PHONY: build deploy up down run logs web

# The host ports docker-compose.yml publishes; override if 5432 or 8080 is taken (and use the same values for
# every target). Exported so `docker compose`, which reads them, sees them.
export CLEAT_PG_PORT  ?= 5432
export CLEAT_API_PORT ?= 8080
# The port `make web` serves the page on (127.0.0.1 only).
export CLEAT_WEB_PORT ?= 3000

build:
	cleat build -o ./out ./

up:
	docker compose up -d
	@echo "worker on http://localhost:$(CLEAT_API_PORT) -- confirm the rate limiter mode with: make logs | grep rate-limiter"

down:
	docker compose down -v

logs:
	docker compose logs cleat-worker

# Deploy the compiled workflow. Needs an API key; the worker prints one on
# first start when no keys exist (see README).
#
# --db points at postgres's HOST-exposed port (docker-compose.yml maps
# $(CLEAT_PG_PORT):5432, 5432 unless you changed it). The worker's own --db uses the docker-network hostname
# "postgres" instead, which only resolves inside the compose network -- this
# make target runs on the host, so it needs the host-reachable form. cleat
# deploy requires a database (or --dry-run) as of cleat#2065; without --db
# here this now fails with "no database configured" rather than silently
# no-op'ing.
deploy: build
	cleat deploy --db "postgres://cleat:cleat@localhost:$(CLEAT_PG_PORT)/cleat?sslmode=disable" \
	  --name my-fullstack-app ./out/submit_order.wasm

# Start one run. Idempotency-Key makes a retry of THIS request return the
# original run instead of creating a second one.
run:
	curl -fsS -X POST http://localhost:$(CLEAT_API_PORT)/api/workflows/my-fullstack-app/start \
	  -H "Authorization: Bearer $$CLEAT_API_KEY" \
	  -H "Idempotency-Key: $$(uuidgen)" \
	  -H "Content-Type: application/json" \
	  -d '{"input":{"item":"widget","qty":1}}'

# Serve web/index.html and the two calls it makes, with the API key added on the server. Needs CLEAT_API_KEY
# (or CLEAT_API_KEY_FILE), the same key `make run` uses. Listens on 127.0.0.1 only: anything that can reach it
# acts as your tenant. See proxy/main.go for exactly what it forwards.
web:
	go run ./proxy -listen 127.0.0.1:$(CLEAT_WEB_PORT) -upstream http://localhost:$(CLEAT_API_PORT)
