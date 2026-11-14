import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const websiteDir = path.resolve(__dirname, '..');
const docsDir = path.join(websiteDir, 'src', 'content', 'docs');
const distDir = path.join(websiteDir, 'dist');
const SITE = 'https://cubepod.cubeplex.ai';

function parseFrontmatter(raw) {
  const match = raw.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n([\s\S]*)$/);
  if (!match) return { title: '', description: '', body: raw };
  const fm = match[1];
  const titleMatch = fm.match(/^title:\s*(.+)$/m);
  const descMatch = fm.match(/^description:\s*(.+)$/m);
  const clean = (s) => (s ? s.trim().replace(/^["']|["']$/g, '') : '');
  return {
    title: clean(titleMatch?.[1]),
    description: clean(descMatch?.[1]),
    body: match[2].trim(),
  };
}

function docFileToUrl(relPath) {
  let slug = relPath.replace(/\.(md|mdx)$/i, '').replace(/\\/g, '/');
  if (slug === 'index') return `${SITE}/`;
  if (slug.endsWith('/index')) slug = slug.slice(0, -'/index'.length);
  return `${SITE}/${slug}/`;
}

const SECTIONS = [
  { prefix: 'getting-started/', label: 'Getting Started' },
  { prefix: 'concepts/', label: 'Core Concepts' },
  { prefix: 'sdks/', label: 'SDKs, CLI & MCP' },
  { prefix: 'deployment/', label: 'Deployment & Operations' },
  { prefix: 'reference/', label: 'Reference' },
  { prefix: 'ceps/', label: 'CubePod Enhancement Proposals (CEPs)' },
];

function collectDocs(dir, base = '') {
  const results = [];
  if (!fs.existsSync(dir)) return results;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const rel = base ? `${base}/${entry.name}` : entry.name;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (rel === 'zh' || rel.startsWith('zh/')) continue;
      results.push(...collectDocs(full, rel));
    } else if (/\.(md|mdx)$/i.test(entry.name)) {
      if (rel === 'index.mdx' || rel === 'index.md') continue;
      results.push({ rel, full });
    }
  }
  return results.sort((a, b) => a.rel.localeCompare(b.rel));
}

function main() {
  if (!fs.existsSync(distDir)) {
    throw new Error(`dist/ not found at ${distDir}. Run astro build first.`);
  }

  const allDocs = collectDocs(docsDir);
  const llmsLines = [
    '# CubePod',
    '',
    '> Persistent Writable RootFS MicroVM Execution Platform for AI Agents (100% E2B-compatible superset). Built with Kata Containers, CSI Raw Block dual volumes (`rootfs-pvc` + `workspace-pvc`), single-threaded Rust `podd-init` OverlayFS pivot, Zero-Export ~1s suspend/resume, `<50ms` `PodWarmPool`, `~1s` CoW `fork`, and Zero-Trust `cpodref_` mTLS egress.',
    '',
    '## Machine-Readable Specs & Resources',
    '',
    `- [Full Documentation Bundle (llms-full.txt)](${SITE}/llms-full.txt): Single-file Markdown corpus of all core guides, SDK docs, Kubernetes CRDs, and Helm values.`,
    `- [OpenAPI 3.1 Specification](${SITE}/openapi.yaml): Canonical E2B-compatible superset REST/SSE API contract.`,
    `- [Interactive API Reference](${SITE}/api/): Browser-based Scalar OpenAPI 3.1 Explorer.`,
    `- [Chinese Documentation](${SITE}/zh/): Simplified Chinese documentation portal.`,
    '',
  ];

  const fullParts = [
    '# CubePod Full Documentation Corpus (v0.4.0)',
    '',
    `Source: ${SITE}`,
    '',
  ];

  for (const sec of SECTIONS) {
    const items = allDocs.filter((d) => d.rel.startsWith(sec.prefix));
    if (items.length === 0) continue;

    llmsLines.push(`## ${sec.label}`);
    llmsLines.push('');

    for (const item of items) {
      const raw = fs.readFileSync(item.full, 'utf8');
      const { title, description, body } = parseFrontmatter(raw);
      const url = docFileToUrl(item.rel);
      const docTitle = title || item.rel;
      llmsLines.push(`- [${docTitle}](${url})${description ? `: ${description}` : ''}`);

      // Keep llms-full.txt high-signal: include full core docs, SDKs, deployment, reference, and CEP index
      if (!item.rel.startsWith('ceps/') || item.rel === 'ceps/index.md') {
        fullParts.push('---');
        fullParts.push(`# ${docTitle}`);
        fullParts.push(`URL: ${url}`);
        fullParts.push('');
        fullParts.push(body);
        fullParts.push('');
      }
    }
    llmsLines.push('');
  }

  fs.writeFileSync(path.join(distDir, 'llms.txt'), llmsLines.join('\n'), 'utf8');
  fs.writeFileSync(path.join(distDir, 'llms-full.txt'), fullParts.join('\n'), 'utf8');
  console.log(`[llms] generated dist/llms.txt and dist/llms-full.txt (${allDocs.length} docs indexed)`);
}

main();
