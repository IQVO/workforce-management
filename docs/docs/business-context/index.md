---
id: index
slug: /business-context
title: Business Context
sidebar_label: Overview
sidebar_position: 1
description: Why this bounded context exists, what it owns, and what it deliberately refuses to own.
---

# Business Context

A fulfillment centre's throughput is a product of two things it can actually
control on the day: **how much work is released**, and **how many trained
people are standing at each process path**. The first belongs to
[wes-work-planning](https://github.com/IQVO/wes-work-planning). The second
is this context.

Four pages here, in reading order:

1. **[Domain vision](./domain-vision.md)** — what problem this service solves
   and why it is a service at all.
2. **[Why it stops at the path boundary](./path-boundary.md)** — the single
   most important design decision in this codebase.
3. **[Two planning horizons](./planning-horizons.md)** — shift-start commitment
   versus intra-shift tracking, and why they are one context rather than two.
4. **[Ubiquitous language](./ubiquitous-language.md)** — the exact vocabulary,
   with the definitions the code actually implements.
