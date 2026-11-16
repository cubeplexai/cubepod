/**
 * Shared top-level navigation and ecosystem links across Starlight docs,
 * splash landing pages, and the standalone Scalar OpenAPI Explorer.
 */

export const ECOSYSTEM_LINKS = [
  { label: 'CubePlex', href: 'https://cubeplex.ai', title: 'CubePlex — AI Agent Workspace' },
  { label: 'CubeLoop', href: 'https://cubeloop.dev', title: 'CubeLoop — Async Agent Runtime' },
];

export function getHeaderNav(locale, pathname = '/') {
  const isZh = locale === 'zh' || locale === 'zh-CN';
  const prefix = isZh ? '/zh' : '';
  const stripped = pathname.replace(/^\/zh(?=\/|$)/, '') || '/';

  const docsLinks = [
    {
      id: 'docs',
      label: isZh ? '文档' : 'Docs',
      href: `${prefix}/getting-started/introduction/`,
      active:
        stripped !== '/' &&
        !stripped.startsWith('/getting-started/quickstart') &&
        !stripped.includes('e2b-compatibility') &&
        !stripped.startsWith('/sdks') &&
        !stripped.startsWith('/deployment') &&
        !stripped.startsWith('/api'),
    },
    {
      id: 'quickstart',
      label: isZh ? '快速上手' : 'Quickstart',
      href: `${prefix}/getting-started/quickstart/`,
      active: stripped.startsWith('/getting-started/quickstart'),
    },
    {
      id: 'e2b',
      label: isZh ? 'E2B 兼容' : 'E2B Compat',
      href: `${prefix}/reference/e2b-compatibility/`,
      active: stripped.includes('e2b-compatibility'),
    },
    {
      id: 'sdks',
      label: 'SDKs',
      href: `${prefix}/sdks/python/`,
      active: stripped.startsWith('/sdks'),
    },
    {
      id: 'deploy',
      label: isZh ? '部署' : 'Deploy',
      href: `${prefix}/deployment/kubernetes-helm/`,
      active: stripped.startsWith('/deployment'),
    },
    {
      id: 'api',
      label: 'API',
      href: `${prefix}/api/`,
      active: stripped.startsWith('/api'),
    },
  ];

  return {
    isZh,
    docsLinks,
    ecosystemLinks: ECOSYSTEM_LINKS,
  };
}
