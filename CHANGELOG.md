# Changelog

## 0.3.0

- Raise the supported toolchain floor to Go 1.25, the oldest upstream-supported Go release.
- Upgrade `golang.org/x/net` to 0.55.0 to remediate four high-severity dependency findings.

## 0.2.17

- Avoid overflow-prone capacity arithmetic when encoding caller-controlled binary payloads.
- Give CI and release-guard workflows explicit read-only repository permissions.
- Keep data-plane and control-plane user-agent versions aligned with the module release.

## 0.2.16

- Add `OperationResource` and `ControlPlane.GetOperation` for durable command polling.
