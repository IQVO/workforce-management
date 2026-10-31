import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';

import HomepageFeatures from '@site/src/components/HomepageFeatures';
import styles from './index.module.css';

function StudyDisclaimer() {
  return (
    <div
      style={{
        background: '#fef3c7',
        color: '#78350f',
        textAlign: 'center',
        padding: '0.6rem 1rem',
        fontSize: '0.9rem',
        borderBottom: '1px solid #f59e0b',
      }}>
      ⚠️ <strong>Study project</strong> — an educational DDD exercise
      following real industry-standard patterns (WMS/WES/WCS,
      certification-gated assignment, CloudEvents). Not a production
      system. Not affiliated with, endorsed by, or representative of the reference operator
      or any other company.
    </div>
  );
}

function HomepageHeader() {
  const {siteConfig} = useDocusaurusContext();
  return (
    <header className={clsx('hero', styles.heroBanner)}>
      <StudyDisclaimer />
      <div className="container">
        <p className={styles.eyebrow}>
          warehouse-systems · Supporting bounded context
        </p>
        <Heading as="h1" className={styles.heroTitle}>
          {siteConfig.title}
        </Heading>
        <p className={styles.heroSubtitle}>
          Who is on shift, on which process path, at what rate — shift-start
          headcount planning and intra-shift labor assignment for a fulfillment
          centre.
        </p>
        <div className={styles.buttons}>
          <Link className="button button--primary button--lg" to="/docs/overview">
            Read the docs
          </Link>
          <Link
            className="button button--secondary button--lg"
            to="/docs/api-reference/rest/workforce-management-api">
            API reference
          </Link>
        </div>
      </div>
    </header>
  );
}

function BoundaryNote() {
  return (
    <section className={styles.boundary}>
      <div className="container">
        <div className={styles.boundaryInner}>
          <Heading as="h2" className={styles.boundaryTitle}>
            It stops at the path boundary — on purpose
          </Heading>
          <p>
            This context never links an associate to a specific task. It records
            which <strong>process path</strong> someone is on, for an interval,
            and nothing finer. Dispatching an individual unit of work to a
            claiming station is a different problem on a different clock —
            seconds, against the minutes-to-hours on which headcount moves
            between paths.
          </p>
          <p>
            Fusing the two would force every task-dispatch policy change to touch
            workforce planning code, and vice versa, even though nothing about
            the labor picture actually changed. So the seam stays here.
          </p>
          <Link to="/docs/adr/0002-stop-at-the-path-boundary">
            Read ADR 0002 →
          </Link>
        </div>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  const {siteConfig} = useDocusaurusContext();
  return (
    <Layout
      title={siteConfig.title}
      description="Shift-start headcount planning per process path and intra-shift labor assignment — a Supporting bounded context of the warehouse-systems platform.">
      <HomepageHeader />
      <main>
        <HomepageFeatures />
        <BoundaryNote />
      </main>
    </Layout>
  );
}
