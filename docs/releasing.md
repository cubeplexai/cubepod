# Releasing CubePod

How to cut a CubePod release. This is a developer process document — do not publish it to `website/`.

User-facing deployment guides live under `website/src/content/docs/deployment/` (English) and `website/src/content/docs/zh/deployment/` (Simplified Chinese). Keep those examples synchronized with the released version.

Agents: load `.agent/skills/release/SKILL.md`, then follow this guide.

## Sequence

1. Inspect the current branch, working tree, `origin/main`, and existing `v*` tags. Never overwrite an existing `v<semver>` tag.
2. From a feature branch off latest `origin/main`, prepare a **version-bump PR** that:
   - Updates every committed version source listed below.
   - Adds the English and Simplified Chinese changelog pages for the version and registers the version at the top of the `Changelog` sidebar in `website/astro.config.mjs`.
3. Verify locally before pushing:
   ```bash
   ./scripts/check-version-consistency.sh v<semver>
   python3 -m unittest discover -s scripts/tests
   python3 scripts/release-notes.py v<semver>
   make fmt && make vet && make test
   python3 -m unittest discover -s sdks/python/tests
   cd sdks/typescript && npm test && cd ../..
   helm lint deploy/helm/cubepod && helm template cubepod-ci deploy/helm/cubepod >/dev/null
   make docs-build
   ```
4. Merge the release PR into `main`.
5. Create annotated tag `v<semver>` on that exact merged commit and push the tag (`git tag -a v<semver> -m "Release v<semver>" && git push origin v<semver>`).
6. Pushing the tag triggers `.github/workflows/release.yml`, which:
   - Validates `scripts/check-version-consistency.sh` and renders release notes from `website/src/content/docs/changelog/<semver>.md` + GitHub generated PR notes.
   - Builds binaries, SDK artifacts, and `SHA256SUMS`, and creates or updates the GitHub Release.
   - Publishes the Python SDK to TestPyPI and PyPI via OIDC, the TypeScript SDK to npm with provenance, the Helm chart to `oci://ghcr.io/cubeplexai/charts`, and multi-arch container images to GHCR.
   - Attaches `release-manifest-v<semver>.yaml` containing `source_commit` and exact `sha256:` image digests.
7. Production deployments should pin the release tags or `release-manifest-v<semver>.yaml` digests rather than `:latest`.

## Version sources

One CubePod semver (example: `0.4.2`) across these committed fields.

**Checked by `scripts/check-version-consistency.sh v<semver>`:**

- `Cargo.toml` → `[workspace.package].version`
- `Cargo.lock` → `podd` and `podd-init` package `version` (refresh with `cargo check --workspace` or update lock entries)
- `pkg/version/version.go` → `var Version = "v<semver>"`
- `api/openapi/cubepod-v1.yaml` → `info.version`
- `cmd/cubepod-controller/main.go` → `-podd-image` default (`ghcr.io/cubeplexai/cubepod-podd:v<semver>`)
- `cmd/cubepod-gateway/main.go` → `-podd-image` default (`ghcr.io/cubeplexai/cubepod-podd:v<semver>`)
- `deploy/helm/cubepod/Chart.yaml` → `version` and `appVersion`
- `deploy/helm/cubepod/values.yaml` → `runtime.poddInjectorImage`, `gateway.image.tag`, `controller.image.tag`, `egress.image.tag`
- `sdks/python/pyproject.toml` → `[project].version`
- `sdks/python/cubepod/__init__.py` → `__version__`
- `sdks/typescript/package.json` → `version`

**User-facing docs (English and Simplified Chinese):**

| File | What to bump |
| :--- | :--- |
| `website/src/content/docs/deployment/kubernetes-helm.md` | Chart `version`/`appVersion`, `poddInjectorImage`, `egress.image.tag` |
| `website/src/content/docs/zh/deployment/kubernetes-helm.md` | Same as English |
| `website/src/content/docs/sdks/python.md` & `zh/sdks/python.md` | Package version |
| `website/src/content/docs/sdks/typescript.md` & `zh/sdks/typescript.md` | Package version |
| `website/src/content/docs/reference/api-overview.md` & `zh/reference/api-overview.md` | OpenAPI spec version |

### Do not bump

- `deploy/helm/cubepod/values.yaml` `runtime.defaultSandboxImage` (`ghcr.io/cubeplexai/cubeplex-sandbox:v0.4.0`) and its mention in `kubernetes-helm.md` unless the external `cubeplex-sandbox` image itself released a matching tag.
- `deploy/k8s/openebs-lvm-operator.yaml` (`controller-gen.kubebuilder.io/version: v0.4.0` upstream annotation).

## Changelog and GitHub release notes

Every release has two documentation pages named after the semver without `v`:

- English: `website/src/content/docs/changelog/<semver>.md`
- Simplified Chinese: `website/src/content/docs/zh/changelog/<semver>.md`

Add `{ label: '<semver>', slug: 'changelog/<semver>' }` at the top of the `Changelog` sidebar group in `website/astro.config.mjs`.

Authoring guidelines:

1. Structure each page into **Features and improvements** (功能与改进), **Fixes** (问题修复, omit if none), and **Before upgrading** (升级前注意事项) — explicitly calling out CRD changes requiring `kubectl apply -f deploy/helm/cubepod/crds/` before `helm upgrade`, image/SDK co-upgrade requirements, and config changes.
2. Keep English and Chinese pages aligned. Never leave a temporary `"unreleased"` label or invent a future publication date.
3. Use plain Markdown with YAML frontmatter (`title`, `slug: changelog/<semver>` or `slug: zh/changelog/<semver>` so Starlight keeps dots in URLs, `description`, `sidebar.label`). Use site-root links like `/deployment/kubernetes-helm/` (or relative `.md` links); avoid MDX components and reference-style links so `scripts/release-notes.py` renders cleanly on GitHub.
4. `scripts/release-notes.py` reads the English page, strips frontmatter, rewrites internal links to `https://cubepod.cubeplex.ai/...`, appends links to both English and Chinese docs pages, and `.github/workflows/release.yml` appends GitHub's generated PR/contributor appendix (categorized by `.github/release.yml`).
5. On a workflow re-run, `.github/workflows/release.yml` replaces the GitHub Release body with the rendered result via `gh release edit --notes-file`, so edit the changelog in git before tagging rather than manually editing the GitHub Release in the UI.
