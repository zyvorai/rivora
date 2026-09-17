# Rivora docs site

Built with [Docusaurus](https://docusaurus.io/) — same shape as
[Netra](https://zyvorai.github.io/netra/). Live at
https://zyvorai.github.io/rivora/.

## Local development

```bash
npm install
npm start
```

## Build

```bash
npm run build
npm run serve   # preview the production build locally
```

## Images

The share card is **not** duplicated into `website/static/` —
`docusaurus.config.ts`'s `staticDirectories` serves `../docs/social` in
place, so the README and this site share the same file.

## Deployment

Automatic: `.github/workflows/pages.yml` builds and publishes on every
push to `main` that touches `website/` or `docs/social/`. Do not use
Docusaurus's built-in `deploy` script (it targets a `gh-pages` branch
this repo does not use).
