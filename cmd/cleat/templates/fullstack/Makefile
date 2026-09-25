.PHONY: build deploy up down run logs

# The host ports docker-compose.yml publishes; override if 5432 or 8080 is taken (and use the same values for
# every target). Exported so `docker compose`, which reads them, sees them.
export CLEAT_PG_PORT  ?= 5432
export CLEAT_API_PORT ?= 8080

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
