# Arctex API emulator

Run the 18 public payment operations from Arctex OpenAPI 0.6.0 against a local simulator with persistent SQLite state. The emulator uses synthetic accounts, addresses, transaction IDs, and signing keys.

## Start locally

Run `docker compose up --build -d --wait` from this directory.

| Endpoint | Default |
| --- | --- |
| API URL | http://127.0.0.1:51080 |
| Development bearer | `emulator-demo` |
| Control URL | http://127.0.0.1:51081 |
| Development control bearer | `emulator-control` |

Set the [Arctex Go SDK](https://github.com/arctex-one/client-go) base URL and bearer to the values above. Start by calling `ListAssets` or `ListAccounts`. The SDK's setup guide explains how to generate the typed Go client.

Compose publishes both listeners on loopback. Use this setup with development credentials, synthetic data, and callback URLs you control. Persisted SQLite state survives a stop and restart. To remove the disposable database, run `docker compose down --volumes`.

## Exercise recovery and callbacks

Use the control endpoint with the control bearer:

- `PUT /control/settings` sets manual mode and can set or advance the clock.
- `PUT /control/tenants/demo` configures callback delivery. Choose HMAC-SHA256 or detached Ed25519.
- `POST /control/deposits` supplies a synthetic deposit.
- `POST /control/transfers/{ref}/step` advances a transfer scenario.
- `POST /control/webhooks/dispatch` dispatches a due callback.

The emulator covers payment operations. The production Arctex API has additional operations. Consumer refund reconciliation should be exercised independently against the integration's own durable ledger.

## Build and test

Go, curl, jq, and sha256sum are required for native builds. Run `make build` and start `./dist/arctex-emulator -data ./dist/demo.sqlite`. Run `make check` for race tests and vet.

Build preparation generates the official SDK from its released source, verifies the published schema checksum, and places those inputs in ignored locations. Tests use the released SDK to submit transfers and verify signed callbacks. Generated Go files and schema snapshots are build outputs.
