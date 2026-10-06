# Workforce Management — documentation site

The Docusaurus source for
<https://iqvo.github.io/workforce-management/>.

Content lives in `docs/` (Markdown), the homepage in `src/pages/index.tsx`, and
the sidebar shape in `sidebars.ts`. The **API Reference → REST API** pages are
generated from `../apis/openapi.yaml` by `docusaurus-plugin-openapi-docs` and
are committed to the repo — do not hand-edit them.

## Local development

```bash
npm ci
npm start          # dev server on http://localhost:3000/workforce-management/
```

## Build

```bash
npm run build      # must exit 0 with no broken-link errors
npm run typecheck
npm run serve      # serve the production build locally
```

## Regenerating the REST API pages

After changing `../apis/openapi.yaml`:

```bash
npm run clean-api-docs
npm run gen-api-docs
npm run build
```

## Deployment

Pushes to `main` that touch `docs/**` trigger `.github/workflows/docs.yml`,
which builds this site and deploys it to GitHub Pages. There is no manual
deploy step.
