# Repository Guidelines

## Project Structure & Module Organization

`main.go` is the CLI entry point. Configuration lives in `config/`; OpenTelemetry logger and TLS setup live in `ologgers/`; Proxmox discovery, journal monitoring, cursors, and metrics live in `pve/`. Version metadata is in `version/`. Keep tests beside their packages as `*_test.go`. Operational resources belong in `goodies/`, while documentation assets belong in `docs/`.

## Build, Test, and Development Commands

- `go build .` builds the local `pve2otelcol` executable.
- `go run . --help` checks CLI wiring without installing the binary.
- `go test ./...` runs all package tests.
- `go test -race ./...` checks concurrent monitor and shutdown paths for races.
- `go vet ./...` performs standard static analysis.
- `gofmt -w main.go config pve ologgers version` formats Go sources before review.

The module targets Go 1.25. Run `go mod tidy` only when imports or dependencies change; commit `go.mod` and `go.sum` updates together.

## Coding Style & Naming Conventions

Follow idiomatic Go and let `gofmt` determine tabs and layout. Use short, lowercase package names; exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and constants should follow the convention already established in their package. Keep operational errors contextual and preserve structured `slog` attributes such as `source`, `command`, and `error`. Prefer small interfaces or injected functions around external commands so Proxmox behavior remains testable.

## Testing Guidelines

Use the standard `testing` package. Name tests `TestBehavior` and use table-driven subtests for validation matrices. Add regression tests in the package affected by a change, including failure, cancellation, and timeout cases where relevant. There is no documented coverage threshold; meaningful behavior coverage is expected. Tests must not require a live Proxmox host or collector.

## Commit & Pull Request Guidelines

Recent history uses brief, single-purpose subjects such as `Use stable source identities`. Write imperative subjects without trailing punctuation, and keep unrelated changes separate. Pull requests should explain the operational effect, list validation commands, and link relevant issues. Update `README.md` or `goodies/pve2otelcol.service` when flags, defaults, deployment, privileges, or shutdown behavior change; include screenshots only for visual changes.

## Security & Configuration Tips

Never commit collector credentials, private keys, cursor data, or local `.env` files. Avoid exposing credential paths in errors. Treat changes to TLS validation, root execution, command invocation, metrics listeners, and systemd hardening as security-sensitive and document their deployment impact.
