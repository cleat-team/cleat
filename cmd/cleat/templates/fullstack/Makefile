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
deploy: build
	cleat deploy --name my-fullstack-app ./out/submit_order.wasm

# Start one run. Idempotency-Key makes a retry of THIS request return the
# original run instead of creating a second one.
run:
	curl -fsS -X POST http://localhost:8080/api/workflows/my-fullstack-app \
	  -H "Authorization: Bearer $$CLEAT_API_KEY" \
	  -H "Idempotency-Key: $$(uuidgen)" \
	  -H "Content-Type: application/json" \
	  -d '{"item":"widget","qty":1}'
