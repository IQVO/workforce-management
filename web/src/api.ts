import { WORKFORCE_API_BASE } from "./config";

/** RFC 7807 problem+json body every error response from workforce-management
 *  returns (see the shared Problem schema in apis/openapi.yaml). Mirrors
 *  order-management's own ApiError shape. */
export interface ProblemDetails {
  type: string;
  title: string;
  status: number;
  detail: string;
  instance?: string;
}

export class ApiError extends Error {
  problem: ProblemDetails | null;
  status: number;

  constructor(status: number, problem: ProblemDetails | null, fallbackMessage: string) {
    super(problem?.detail || problem?.title || fallbackMessage);
    this.status = status;
    this.problem = problem;
  }
}

async function parseProblemOrThrow(res: Response): Promise<void> {
  let problem: ProblemDetails | null = null;
  try {
    problem = (await res.json()) as ProblemDetails;
  } catch {
    // non-JSON error body -- fall through with problem = null
  }
  throw new ApiError(res.status, problem, `${res.status} ${res.statusText}`);
}

/** A fresh Idempotency-Key: one random (v4) UUID per submission (the
 *  fleet-wide ADR-0027 client convention, e.g. order-management's
 *  newIdempotencyKey). This remote had no POST screens before, so "uuid
 *  per submission" is the convention adopted here. */
export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

/**
 * POST call for this remote's supervisor write endpoints
 * (POST /associates/{id}/certifications and /associates/{id}/assignments).
 *
 * The idempotencyKey parameter is REQUIRED on purpose: workforce-management
 * runs with Postgres in every real deployment and rejects a missing
 * Idempotency-Key on /associates/{id}/assignments with 400
 * idempotency-key-required (ADR-0027), so this console never issues a
 * POST without one -- the type system enforces the convention. Callers
 * mint one per logical submission and may reuse it when retrying that
 * same submission after a network failure (no answer arrived).
 *
 * Parses an RFC 7807 problem+json body on failure so the screens can
 * surface the exact domain-error detail (e.g. certification-required)
 * instead of a generic "request failed".
 */
export async function apiPost<TResponse>(
  path: string,
  body: unknown,
  idempotencyKey: string,
): Promise<TResponse> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "Idempotency-Key": idempotencyKey,
  };
  const res = await fetch(`${WORKFORCE_API_BASE}${path}`, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });
  if (!res.ok) await parseProblemOrThrow(res);
  // certifyAssociate answers 204 with no body.
  if (res.status === 204) return undefined as TResponse;
  return (await res.json()) as TResponse;
}

/** Upper bound on pages followed by fetchAllPages, so a misbehaving server
 *  that keeps returning a nextCursor cannot loop the browser forever. */
const MAX_PAGES = 50;

/** GET a paged collection and follow nextCursor until it is absent (fleet
 *  REST guideline 5.2/5.3): reads `items`, never a bare array. `url` may
 *  already carry a query string; the cursor is appended with the same
 *  filters on every page request. */
export async function fetchAllPages<T>(url: string, signal?: AbortSignal): Promise<T[]> {
  const out: T[] = [];
  let cursor: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const u = cursor
      ? `${url}${url.includes("?") ? "&" : "?"}cursor=${encodeURIComponent(cursor)}`
      : url;
    const res = await fetch(u, { signal });
    if (!res.ok) await parseProblemOrThrow(res);
    const page = (await res.json()) as { items: T[]; nextCursor?: string };
    out.push(...page.items);
    if (!page.nextCursor) return out;
    cursor = page.nextCursor;
  }
  throw new Error(`collection ${url} did not end after ${MAX_PAGES} pages`);
}
