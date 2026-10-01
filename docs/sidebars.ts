import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';
import restApiSidebar from './docs/api-reference/rest/sidebar';

/**
 * The six top-level categories are fixed across every warehouse-systems
 * documentation site, in this order, so the five sites read as one family.
 */
const sidebars: SidebarsConfig = {
  docsSidebar: [
    {
      type: 'category',
      label: 'Overview',
      collapsed: false,
      link: {type: 'doc', id: 'overview/index'},
      items: [
        'overview/quickstart',
        'overview/architecture',
      ],
    },
    {
      type: 'category',
      label: 'Business Context',
      collapsed: false,
      link: {type: 'doc', id: 'business-context/index'},
      items: [
        'business-context/domain-vision',
        'business-context/path-boundary',
        'business-context/planning-horizons',
        'business-context/ubiquitous-language',
      ],
    },
    {
      type: 'category',
      label: 'Domain-Driven Design',
      collapsed: false,
      link: {type: 'doc', id: 'ddd/index'},
      items: [
        'ddd/subdomain-classification',
        'ddd/aggregates',
        'ddd/invariants',
        'ddd/domain-events',
        'ddd/context-relationships',
      ],
    },
    {
      type: 'category',
      label: 'API Reference',
      collapsed: false,
      link: {type: 'doc', id: 'api-reference/index'},
      items: [
        {
          type: 'category',
          label: 'REST API',
          link: {type: 'doc', id: 'api-reference/rest/workforce-management-api'},
          items: restApiSidebar.slice(1),
        },
        'api-reference/events',
        'api-reference/errors',
      ],
    },
    {
      type: 'category',
      label: 'Ecosystem',
      collapsed: false,
      link: {type: 'doc', id: 'ecosystem/index'},
      items: [
        'ecosystem/context-map',
        'ecosystem/siblings',
        'ecosystem/integration',
      ],
    },
    {
      type: 'category',
      label: 'AI Ecosystem (MCP)',
      collapsed: false,
      items: ['mcp/governance-charter'],
    },
    {
      type: 'category',
      label: 'Analytics (Data Product)',
      collapsed: false,
      items: ['analytics/labor-report'],
    },
    {
      type: 'category',
      label: 'Architecture Decision Records',
      collapsed: false,
      link: {type: 'doc', id: 'adr/index'},
      items: [
        'adr/0001-hexagonal-ports-and-adapters',
        'adr/0002-stop-at-the-path-boundary',
        'adr/0003-certification-gated-single-active-assignment',
        'adr/0004-kafka-integration-events-and-cloudevents-catalog',
        'adr/0005-rfc-7807-problem-details',
        'adr/0006-godog-bdd-acceptance-tests',
        'adr/0007-arch-go-architecture-fitness-tests',
        'adr/0008-mcp-inbound-adapter',
        'adr/0009-hazmat-certification-via-existing-path-gating',
        'adr/0010-analytical-data-product',
        'adr/0011-adopt-fleet-mfe-console-architecture',
        'adr/0012-measured-rate-feed-for-propose-path-plan',
        'adr/0013-process-path-catalogue-validation',
        'adr/0014-installed-capacity-ceiling',
        'adr/0015-standard-metrics-convention',
        'adr/0016-transactional-outbox',
        'adr/0017-adopt-fleet-rest-identity',
        'adr/0018-remove-fleet-rest-identity',
        'adr/0019-labor-performance-cache-consumer',
        'adr/0020-idle-share-staffing-signal',
        'adr/0021-optimistic-concurrency-version-column',
        'adr/0022-resilience-circuit-breakers-retry-dlq-shutdown',
        'adr/0023-kafka-integration-producer-partition-key',
        'adr/0024-horizontal-autoscaling-and-pgxpool-tuning',
        'adr/0025-migrations-direct-postgres-connection',
        'adr/0026-cloudevents-mandatory-event-envelope',
      ],
    },
  ],
};

export default sidebars;
