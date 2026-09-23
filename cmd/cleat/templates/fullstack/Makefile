.PHONY: build deploy up down run logs

build:
	cleat build -o ./out ./

up:
	docker compose up -d
	@echo "worker on http://localhost:8080 -- confirm the rate limiter mode with: make logs | grep rate-limiter"

down:
	docker compose down -v

logs:
	docker compose logs cleat-worker

# Deploy the compiled workflow. Needs an API key; the worker prints one on
# first start when no keys exist (see README).
#
# --db points at postgres's HOST-exposed port (docker-compose.yml maps
# 5432:5432). The worker's own --db uses the docker-network hostname
# "postgres" instead, which only resolves inside the compose network -- this
# make target runs on the host, so it needs the host-reachable form. cleat
# deploy requires a database (or --dry-run) as of cleat#2065; without --db
# here this now fails with "no database configured" rather than silently
# no-op'ing.
deploy: build
	cleat deploy --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
	  --name my-fullstack-app ./out/submit_order.wasm

# Start one run. Idempotency-Key makes a retry of THIS request return the
# original run instead of creating a second one.
run:
	curl -fsS -X POST http://localhost:8080/api/workflows/my-fullstack-app/start \
	  -H "Authorization: Bearer $$CLEAT_API_KEY" \
	  -H "Idempotency-Key: $$(uuidgen)" \
	  -H "Content-Type: application/json" \
	  -d '{"input":{"item":"widget","qty":1}}'
