# Repository instructions

This repository owns the published Go client and agent SDK module for supported Thalovant public API and HiveMind runtime contracts. Read the platform contracts in `../infra-manifests/docs/thalovant-platform/` when available.

Rules:

- Preserve semantic-version compatibility with the documented Go and Thalovant API support window.
- Update exported types, implementation, examples, tests, release notes, `VERSION`, and public documentation together for observable contract changes.
- Consume additive server behavior only after compatible server support exists.
- Never publish credentials, identity files, or generated secrets.
- Do not create a release for internal platform changes with no Go SDK impact; record `no SDK impact` in the coordinated change instead.
- Validate a clean module consumer before declaring a tagged release complete.
- Update affected `docs.thalovant.com` SDK pages in the same release train.

Validate with `test -z "$(gofmt -l .)"`, `go vet ./...`, and `go test ./...`. A published release also requires a clean module to resolve `github.com/thalovant/thalovant-go-sdk@<version>` and compile a basic import.

Rollback with a corrected patch tag. If a released module version is unusable, publish a `retract` directive in a later compatible module version rather than rewriting an existing tag.
