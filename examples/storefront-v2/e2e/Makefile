# storefront v2 end-to-end (hand-written layer, boundaries.md §8). See README.md.
COMPOSE := docker compose -f compose.yaml
IMAGES  := sf2-e2e/inventory:local sf2-e2e/payment:local sf2-e2e/notification:local sf2-e2e/order:local

.PHONY: e2e up driver down logs clean

e2e:              ## clean stack → driver → down -v (exit code = the driver's); prints the wall time
	@start=$$(date +%s); \
	$(COMPOSE) down -v --remove-orphans >/dev/null 2>&1; \
	if ! $(COMPOSE) up --build --wait --quiet-pull; then $(COMPOSE) logs --tail 50; $(COMPOSE) down -v --remove-orphans; exit 1; fi; \
	up=$$(date +%s); \
	(cd driver && go run .); rc=$$?; \
	if [ $$rc -ne 0 ]; then $(COMPOSE) logs --tail 80 inventory payment notification order; fi; \
	$(COMPOSE) down -v --remove-orphans >/dev/null 2>&1; \
	end=$$(date +%s); \
	echo "make e2e: $$((end - start))s (stack up $$((up - start))s, driver + down $$((end - up))s), exit $$rc"; \
	exit $$rc

up:               ## build + start the stack (stays up: make driver, make logs, make down)
	@$(COMPOSE) up --build --wait --quiet-pull

driver:           ## run the driver against a running stack (re-runnable: SKUs / order ids are unique per run)
	@cd driver && go run .

logs:
	@$(COMPOSE) logs --tail 200 inventory payment notification order

down:             ## stop and remove the containers and volumes
	@$(COMPOSE) down -v --remove-orphans

clean: down       ## also remove the four images built here (never the shared rabbitmq / postgres images)
	@docker image rm $(IMAGES) >/dev/null 2>&1 || true
