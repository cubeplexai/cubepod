---
name: release
description: Prepare and publish CubePod releases across Rust crates, Go control plane, Python/TypeScript SDKs, Helm chart, GHCR images, and bilingual website changelogs. Use when cutting a release, bumping versions, authoring release changelogs, or troubleshooting release workflows.
---

# CubePod release workflow

Use this skill when preparing or publishing a CubePod release. Exact version sources, verification commands, and changelog rules live in [docs/releasing.md](../../../docs/releasing.md) — **read that file before acting**.

## Required sequence

1. Inspect the current branch, working tree, `origin/main`, and existing `v*` tags.
2. Prepare a version-bump PR that updates all version sources in `docs/releasing.md` (Rust, Go, OpenAPI, Helm, Python/TS SDKs, and EN/ZH docs), adds `website/src/content/docs/changelog/<semver>.md` and `website/src/content/docs/zh/changelog/<semver>.md`, and registers the version at the top of `Changelog` in `website/astro.config.mjs`. Put upgrade instructions (especially CRD updates) after features and fixes.
3. Run `./scripts/check-version-consistency.sh v<semver>`, `python3 -m unittest discover -s scripts/tests`, `python3 scripts/release-notes.py v<semver>`, `make fmt && make vet && make test`, SDK tests, `helm lint`, and `make docs-build`. Review the rendered release notes output.
4. Merge the PR into `main`.
5. Create an annotated tag `v<semver>` on the merged commit and push it to trigger `.github/workflows/release.yml`.
6. Verify the workflow publishes binaries, PyPI, npm, Helm OCI chart, GHCR images, and `release-manifest-v<semver>.yaml`, and updates the GitHub Release body from the changelog.

## Guardrails

- Never overwrite an existing `v<semver>` tag.
- Never publish developer release checklists inside `website/src/content/docs/`.
- If a release workflow image or package publish fails, fix the cause and cut the next patch release rather than mutating an existing tag.
