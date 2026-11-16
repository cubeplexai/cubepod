import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { remarkMermaid } from './src/plugins/remark-mermaid.mjs';
import { GA_INLINE_SCRIPT, GA_SCRIPT_SRC } from './src/config/analytics.mjs';

export default defineConfig({
  site: 'https://cubepod.dev',
  markdown: {
    remarkPlugins: [remarkMermaid],
  },
  integrations: [
    starlight({
      title: {
        en: 'CubePod',
        'zh-CN': 'CubePod',
      },
      description: 'Persistent Writable RootFS MicroVM Execution Platform for AI Agents (E2B-Compatible Superset)',
      logo: {
        dark: './src/assets/logo-dark.svg',
        light: './src/assets/logo-light.svg',
        replacesTitle: true,
      },
      favicon: '/favicon.svg',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/cubeplexai/cubepod',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/cubeplexai/cubepod/edit/main/website/',
      },
      customCss: ['./src/styles/custom.css'],
      components: {
        Head: './src/components/HeadWithMermaid.astro',
        Header: './src/components/Header.astro',
      },
      head: [
        {
          tag: 'link',
          attrs: {
            rel: 'preconnect',
            href: 'https://fonts.googleapis.com',
          },
        },
        {
          tag: 'link',
          attrs: {
            rel: 'preconnect',
            href: 'https://fonts.gstatic.com',
            crossorigin: '',
          },
        },
        {
          tag: 'link',
          attrs: {
            rel: 'stylesheet',
            href: 'https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500;600;700&family=Plus+Jakarta+Sans:wght@600;700;800&display=swap',
          },
        },
        {
          tag: 'script',
          content: `
            try {
              if (!localStorage.getItem('starlight-theme')) {
                localStorage.setItem('starlight-theme', 'dark');
                document.documentElement.dataset.theme = 'dark';
              }
            } catch {}
          `,
        },
        {
          tag: 'meta',
          attrs: {
            property: 'og:image',
            content: 'https://cubepod.dev/og-banner.png',
          },
        },
        {
          tag: 'meta',
          attrs: {
            property: 'og:image:type',
            content: 'image/png',
          },
        },
        {
          tag: 'meta',
          attrs: {
            property: 'og:image:width',
            content: '1200',
          },
        },
        {
          tag: 'meta',
          attrs: {
            property: 'og:image:height',
            content: '630',
          },
        },
        {
          tag: 'meta',
          attrs: {
            property: 'og:image:alt',
            content: 'CubePod — Sandbox Built for Long-Lived Autonomous Agents',
          },
        },
        {
          tag: 'meta',
          attrs: {
            name: 'twitter:card',
            content: 'summary_large_image',
          },
        },
        {
          tag: 'meta',
          attrs: {
            name: 'twitter:image',
            content: 'https://cubepod.dev/og-banner.png',
          },
        },
        {
          tag: 'meta',
          attrs: {
            name: 'twitter:image:alt',
            content: 'CubePod — Sandbox Built for Long-Lived Autonomous Agents',
          },
        },
        {
          tag: 'script',
          attrs: {
            async: true,
            src: GA_SCRIPT_SRC,
          },
        },
        {
          tag: 'script',
          content: GA_INLINE_SCRIPT,
        },
      ],
      defaultLocale: 'root',
      locales: {
        root: {
          label: 'English',
          lang: 'en',
        },
        zh: {
          label: '简体中文',
          lang: 'zh-CN',
        },
      },
      sidebar: [
        {
          label: 'Getting Started',
          translations: { 'zh-CN': '快速入门' },
          items: [
            {
              label: 'Introduction',
              translations: { 'zh-CN': '平台简介' },
              slug: 'getting-started/introduction',
            },
            {
              label: '5-Minute Quickstart',
              translations: { 'zh-CN': '5 分钟快速上手' },
              slug: 'getting-started/quickstart',
            },
            {
              label: 'E2B Compatibility Guide',
              translations: { 'zh-CN': 'E2B 兼容接入' },
              slug: 'getting-started/e2b-compatibility',
            },
          ],
        },
        {
          label: 'Core Concepts',
          translations: { 'zh-CN': '核心原理与架构' },
          items: [
            {
              label: 'System Architecture',
              translations: { 'zh-CN': '系统总体架构' },
              slug: 'concepts/architecture',
            },
            {
              label: 'Dual-Volume Persistence',
              translations: { 'zh-CN': '双卷持久化与 RootFS 重置' },
              slug: 'concepts/dual-volume-persistence',
            },
            {
              label: 'In-Guest Init & podd Daemon',
              translations: { 'zh-CN': 'Guest 内挂载与 podd 守护进程' },
              slug: 'concepts/in-guest-init-and-podd',
            },
            {
              label: 'Lifecycle, WarmPool & CoW Fork',
              translations: { 'zh-CN': '生命周期、预热池与 1s CoW 克隆' },
              slug: 'concepts/lifecycle-warmpool-fork',
            },
            {
              label: 'Zero-Trust Egress & Secrets',
              translations: { 'zh-CN': '零信任出站与凭据隔离' },
              slug: 'concepts/zero-trust-egress',
            },
            {
              label: 'Interactive Desktop & Panel Proxy',
              translations: { 'zh-CN': '桌面环境与签名 Panel 代理' },
              slug: 'concepts/desktop-and-panel-proxy',
            },
          ],
        },
        {
          label: 'SDKs, CLI & MCP',
          translations: { 'zh-CN': 'SDK、CLI 与 MCP' },
          items: [
            {
              label: 'Python SDK',
              translations: { 'zh-CN': 'Python SDK' },
              slug: 'sdks/python',
            },
            {
              label: 'TypeScript SDK',
              translations: { 'zh-CN': 'TypeScript SDK' },
              slug: 'sdks/typescript',
            },
            {
              label: 'Go SDK & CLI',
              translations: { 'zh-CN': 'Go SDK 与命令行工具' },
              slug: 'sdks/go-and-cli',
            },
            {
              label: 'MCP Server Integration',
              translations: { 'zh-CN': 'MCP Server 集成' },
              slug: 'sdks/mcp-server',
            },
            {
              label: 'Template Catalog & Builds',
              translations: { 'zh-CN': '沙箱模板目录与自定义构建' },
              slug: 'sdks/templates',
            },
          ],
        },
        {
          label: 'Deployment & Operations',
          translations: { 'zh-CN': '部署与生产运维' },
          items: [
            {
              label: 'Local Docker Compose',
              translations: { 'zh-CN': '本地 Docker Compose 部署' },
              slug: 'deployment/docker-compose',
            },
            {
              label: 'Kubernetes & Helm Chart',
              translations: { 'zh-CN': 'Kubernetes 与 Helm 生产部署' },
              slug: 'deployment/kubernetes-helm',
            },
            {
              label: 'Kata MicroVM & Block CSI',
              translations: { 'zh-CN': 'Kata 微虚拟机与块存储 CSI' },
              slug: 'deployment/kata-and-block-storage',
            },
          ],
        },
        {
          label: 'Reference',
          translations: { 'zh-CN': '参考手册' },
          items: [
            {
              label: 'API Overview & Streaming',
              translations: { 'zh-CN': 'API 概览与流式协议' },
              slug: 'reference/api-overview',
            },
            {
              label: 'E2B Compatibility',
              translations: { 'zh-CN': 'E2B 兼容性与差异' },
              slug: 'reference/e2b-compatibility',
            },
            {
              label: 'Interactive OpenAPI Explorer',
              translations: { 'zh-CN': '交互式 OpenAPI 控制台' },
              link: '/api/',
            },
            {
              label: 'Kubernetes CRDs (v1alpha1)',
              translations: { 'zh-CN': 'Kubernetes CRD 字段字典' },
              slug: 'reference/crds',
            },
            {
              label: 'Helm Chart Values',
              translations: { 'zh-CN': 'Helm Chart 参数全表' },
              slug: 'reference/helm-values',
            },
            {
              label: 'llms.txt (AI Index)',
              translations: { 'zh-CN': 'llms.txt (AI 文档索引)' },
              link: 'https://cubepod.dev/llms.txt',
            },
          ],
        },
        {
          label: 'Enhancement Proposals (CEPs)',
          translations: { 'zh-CN': '架构演进提案 (CEPs)' },
          collapsed: true,
          autogenerate: { directory: 'ceps' },
        },
        {
          label: 'Changelog',
          translations: { 'zh-CN': '更新日志' },
          items: [
            { label: '0.5.4', slug: 'changelog/0.5.4' },
            { label: '0.5.3', slug: 'changelog/0.5.3' },
            { label: '0.5.2', slug: 'changelog/0.5.2' },
            { label: '0.5.1', slug: 'changelog/0.5.1' },
            { label: '0.5.0', slug: 'changelog/0.5.0' },
            { label: '0.4.2', slug: 'changelog/0.4.2' },
            { label: '0.4.1', slug: 'changelog/0.4.1' },
            { label: '0.4.0', slug: 'changelog/0.4.0' },
          ],
        },
      ],
    }),
  ],
});
