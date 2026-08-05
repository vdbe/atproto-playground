# Developer Guide

1. Install the prerequisites: Go 1.26.5 or later, Docker with Compose, OpenSSL, and curl.

2. Create the local configuration:

   ```bash
   cp .env.example .env
   ```

3. Generate the three blank PDS secrets independently. Run this command three times and assign a different result to each of `PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX`, `PDS_JWT_SECRET`, and `PDS_ADMIN_PASSWORD` in `.env`:

   ```bash
   openssl rand -hex 32
   ```

4. Start the stack and verify its health checks:

   ```bash
   docker compose -f deployments/compose.yaml up --build --wait
   curl --fail http://localhost:2582/_health
   curl --fail http://localhost:2583/xrpc/_health
   curl --fail http://localhost:2584/healthz
   curl --fail http://localhost:5050/misc/ping
   ```

5. Install [Goat](https://github.com/bluesky-social/goat) and create the account configured in `.env`:

   ```bash
   go install github.com/bluesky-social/goat@latest
   ./scripts/create-local-account.sh
   ```

6. Run the bot. Stop it with `Ctrl-C` or another `SIGINT`/`SIGTERM` signal.

   ```bash
   go run ./cmd/bot
   ```

7. Use the local services:

   - Grafana dashboard: <http://localhost:3000/d/atproto-dev-services> (`admin` / `admin`)
   - PDS: <http://localhost:2583/>
   - PLC directory: <http://localhost:2582/>
   - PDSls repository listing: <http://localhost:2584/localhost:2583>
   - pgAdmin: <http://localhost:5050/> (defaults from `.env`: `admin@example.com` / `test-only-pgadmin-password`)

8. Run ordinary tests or race-enabled tests. The race command removes the race detector's exit delay.

   ```bash
   go test ./...
   go test -race ./...
   ```

9. Stop the Compose stack while preserving its named volumes:

   ```bash
   docker compose -f deployments/compose.yaml down
   ```

10. To fully reset the isolated environment, including local accounts and persisted service data, remove the named volumes:

    ```bash
    docker compose -f deployments/compose.yaml down --volumes
    ```
