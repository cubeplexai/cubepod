import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const websiteDir = path.resolve(__dirname, '..');
const repoRoot = path.resolve(websiteDir, '..');
const distDir = path.join(websiteDir, 'dist');
const enhRoot = path.join(repoRoot, 'docs', 'enhancements');

function assertFile(relPath) {
  const full = path.join(distDir, relPath);
  if (!fs.existsSync(full)) {
    throw new Error(`Missing required build artifact: dist/${relPath}`);
  }
  const stat = fs.statSync(full);
  if (stat.size === 0) {
    throw new Error(`Build artifact is empty: dist/${relPath}`);
  }
}

function collectHtmlFiles(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...collectHtmlFiles(full));
    } else if (entry.name.endsWith('.html')) {
      out.push(full);
    }
  }
  return out;
}

function targetExistsInDist(urlPath) {
  // Normalize leading slash and decode percent-encoding
  let clean = decodeURIComponent(urlPath.split('#')[0].split('?')[0]);
  if (!clean.startsWith('/')) return true;
  if (clean === '/') {
    return fs.existsSync(path.join(distDir, 'index.html'));
  }
  const rel = clean.slice(1);
  const direct = path.join(distDir, rel);
  if (fs.existsSync(direct) && fs.statSync(direct).isFile()) return true;
  const idx = path.join(distDir, rel, 'index.html');
  if (fs.existsSync(idx) && fs.statSync(idx).isFile()) return true;
  if (rel.endsWith('.html') && fs.existsSync(direct)) return true;
  return false;
}

function main() {
  if (!fs.existsSync(distDir)) {
    throw new Error(`dist/ does not exist at ${distDir}`);
  }

  const requiredFiles = [
    'index.html',
    'zh/index.html',
    '404.html',
    'api/index.html',
    'zh/api/index.html',
    'openapi.yaml',
    'vendor/scalar-api-reference.js',
    'llms.txt',
    'llms-full.txt',
    'pagefind/pagefind.js',
    'reference/crds/index.html',
    'zh/reference/crds/index.html',
    'reference/helm-values/index.html',
    'zh/reference/helm-values/index.html',
    'ceps/index.html',
    'zh/ceps/index.html',
  ];

  // Require all 22 CEPs in dist/ceps/
  const cepDirs = fs
    .readdirSync(enhRoot, { withFileTypes: true })
    .filter((d) => d.isDirectory() && /^\d{4}-/.test(d.name))
    .map((d) => d.name)
    .sort();

  for (const cepDir of cepDirs) {
    requiredFiles.push(`ceps/${cepDir}/index.html`);
  }

  for (const f of requiredFiles) {
    assertFile(f);
  }
  console.log(`[verify] verified ${requiredFiles.length} required artifacts & ${cepDirs.length} CEP pages`);

  const htmlFiles = collectHtmlFiles(distDir);
  const brokenLinks = [];
  const attrRegex = /\b(?:href|src)=["'](\/[^"']*)["']/g;

  for (const htmlFile of htmlFiles) {
    const relHtml = path.relative(distDir, htmlFile);
    const content = fs.readFileSync(htmlFile, 'utf8');
    let match;
    while ((match = attrRegex.exec(content)) !== null) {
      const ref = match[1];
      if (ref.startsWith('//')) continue; // protocol-relative external URL
      if (!targetExistsInDist(ref)) {
        brokenLinks.push({ page: relHtml, ref });
      }
    }
  }

  if (brokenLinks.length > 0) {
    console.error(`[verify] found ${brokenLinks.length} broken internal link(s):`);
    for (const b of brokenLinks.slice(0, 40)) {
      console.error(`  - ${b.page} -> ${b.ref}`);
    }
    process.exit(1);
  }

  console.log(`[verify] crawled ${htmlFiles.length} HTML files — zero broken internal links or missing assets`);
}

main();
