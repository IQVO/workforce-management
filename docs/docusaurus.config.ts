import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import type * as OpenApiPlugin from 'docusaurus-plugin-openapi-docs';

const config: Config = {
  title: 'Workforce Management',
  tagline:
    'Who is on shift, on which process path, at what rate — headcount planning and intra-shift labor assignment, stopping deliberately at the path boundary.',
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
    faster: true,
  },

  url: 'https://iqvo.github.io',
  baseUrl: '/workforce-management/',

  organizationName: 'IQVO',
  projectName: 'workforce-management',
  deploymentBranch: 'gh-pages',
  trailingSlash: false,

  onBrokenLinks: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  themes: ['@docusaurus/theme-mermaid', 'docusaurus-theme-openapi-docs'],

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: 'docs',
          editUrl:
            'https://github.com/IQVO/workforce-management/tree/main/docs/',
          docItemComponent: '@theme/ApiItem',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  plugins: [
    [
      'docusaurus-plugin-openapi-docs',
      {
        id: 'api',
        docsPluginId: 'classic',
        config: {
          workforce: {
            specPath: '../apis/openapi.yaml',
            outputDir: 'docs/api-reference/rest',
            downloadUrl:
              'https://raw.githubusercontent.com/IQVO/workforce-management/main/apis/openapi.yaml',
            sidebarOptions: {
              groupPathsBy: 'tag',
              categoryLinkSource: 'tag',
            },
          } satisfies OpenApiPlugin.Options,
        },
      },
    ],
  ],

  themeConfig: {
    image: 'img/logo.svg',
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Workforce Management',
      logo: {
        alt: 'Workforce Management',
        src: 'img/logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Documentation',
        },
        {
          to: '/docs/api-reference/rest/workforce-management-api',
          position: 'left',
          label: 'API',
        },
        {
          to: '/docs/adr',
          position: 'left',
          label: 'ADRs',
        },
        {
          href: 'https://github.com/IQVO/workforce-management',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Documentation',
          items: [
            {label: 'Overview', to: '/docs/overview'},
            {label: 'Business Context', to: '/docs/business-context/domain-vision'},
            {label: 'Domain-Driven Design', to: '/docs/ddd/subdomain-classification'},
            {label: 'Architecture Decisions', to: '/docs/adr'},
          ],
        },
        {
          title: 'Contracts',
          items: [
            {label: 'REST API', to: '/docs/api-reference/rest/workforce-management-api'},
            {label: 'Domain Events', to: '/docs/api-reference/events'},
            {
              label: 'openapi.yaml',
              href: 'https://github.com/IQVO/workforce-management/blob/main/apis/openapi.yaml',
            },
            {
              label: 'asyncapi.yaml',
              href: 'https://github.com/IQVO/workforce-management/blob/main/apis/asyncapi.yaml',
            },
          ],
        },
        {
          title: 'warehouse-systems',
          items: [
            {label: 'inventory-storage', href: 'https://github.com/IQVO/inventory-storage'},
            {label: 'wes-work-planning', href: 'https://github.com/IQVO/wes-work-planning'},
            {label: 'fulfillment-execution', href: 'https://github.com/IQVO/fulfillment-execution'},
            {label: 'facility-layout', href: 'https://github.com/IQVO/facility-layout'},
          ],
        },
      ],
      copyright:
        'Workforce Management — a bounded context of the warehouse-systems platform.',
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['go', 'bash', 'json', 'sql', 'yaml', 'gherkin'],
    },
    mermaid: {
      theme: {light: 'neutral', dark: 'dark'},
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
