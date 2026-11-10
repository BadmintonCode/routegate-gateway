# Contributing to RouteGate

Thanks for your interest in contributing. This document covers the basics;
nailing down details is part of reaching v0.1.0.

## Developer Certificate of Origin (DCO)

This project uses the DCO: every commit must be signed off to certify that you
have the right to submit it under the [Apache-2.0 license](LICENSE). There is
no separate CLA.

```bash
git commit -s -m "feat: your change"
```

The sign-off line must match the commit author email.

## Branch and pull request workflow

- `master` is the production branch; `test` is the integration branch.
- Work on short-lived branches: `feat/<name>`, `fix/<name>`, `chore/<name>`,
  `docs/<name>`.
- Open pull requests into `test` first. CI must pass. Promote validated
  commits to `master` with a separate PR.
- Direct pushes to `master` or `test` are outside the normal workflow.

## Commit style

Imperative subjects with a type prefix: `feat:`, `fix:`, `docs:`, `test:`,
`chore:`. Keep commits focused.

## Code expectations

- Go: standard library first. Dependencies require explicit justification —
  the whitelist currently is the MySQL driver, `golang.org/x/crypto`, and
  `golang-jwt` (JWT fallback only).
- `gofmt` formatting, `go vet` clean, `-race` tests green in CI.
- Security-sensitive code paths (path normalization, wildcard matching,
  certificate verification) require fuzz or table tests covering the bypass
  cases listed in ADR-0001 §17.
- Never commit secrets, keys, certificates, DSNs, or `.env` files. Generated
  test fixtures live under `testdata/` only.

## Reporting issues

Open a GitHub issue with reproduction steps. For vulnerabilities, follow
[SECURITY.md](SECURITY.md) — do not open a public issue.
