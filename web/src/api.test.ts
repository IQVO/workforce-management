import { afterEach, describe, expect, it, vi } from "vitest";
import { WORKFORCE_API_BASE } from "./config";
import { apiPost, ApiError, newIdempotencyKey } from "./api";

/** One request apiPost made through global fetch. */
interface MockCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}

/** Replaces global fetch with a stub returning `reply` for every call and
 *  recording what was sent (the inbound-receiving fetchMock pattern,
 *  inlined here because this remote's suite has no msw). */
function stubFetch(reply: (call: MockCall) => Response | Promise<Response>): MockCall[] {
  const calls: MockCall[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const call: MockCall = {
        url: String(input),
        method: (init?.method ?? "GET").toUpperCase(),
        headers: (init?.headers ?? {}) as Record<string, string>,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      };
      calls.push(call);
      // Clone so a static Response can answer repeated calls (a body can
      // only be read once).
      const res = await reply(call);
      return res.clone();
    }),
  );
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("apiPost", () => {
  it("POSTs the body to the API base with Content-Type and the Idempotency-Key", async () => {
    const calls = stubFetch(() => new Response(null, { status: 204 }));

    await apiPost("/associates/assoc-1/certifications", { certification: "hazmat" }, "key-1");

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(`${WORKFORCE_API_BASE}/associates/assoc-1/certifications`);
    expect(calls[0].method).toBe("POST");
    expect(calls[0].headers["Content-Type"]).toBe("application/json");
    expect(calls[0].headers["Idempotency-Key"]).toBe("key-1");
    expect(calls[0].body).toEqual({ certification: "hazmat" });
  });

  it("resolves undefined on 204 (certifyAssociate returns no body)", async () => {
    stubFetch(() => new Response(null, { status: 204 }));

    const result = await apiPost("/associates/assoc-1/certifications", { certification: "pack" }, "key-2");

    expect(result).toBeUndefined();
  });

  it("parses the RFC 7807 problem body into an ApiError carrying its detail", async () => {
    stubFetch(
      () =>
        new Response(
          JSON.stringify({
            type: "https://errors.workforce-management.warehouse-systems.dev/certification-required",
            title: "Associate lacks the certification required for this path",
            status: 409,
            detail: "associate lacks the certification required for this path",
            instance: "/associates/assoc-1/assignments",
          }),
          { status: 409, headers: { "Content-Type": "application/problem+json" } },
        ),
    );

    const err = await apiPost(
      "/associates/assoc-1/assignments",
      { pathId: "pack" },
      "key-3",
    ).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    const apiError = err as ApiError;
    expect(apiError.status).toBe(409);
    expect(apiError.message).toBe("associate lacks the certification required for this path");
    expect(apiError.problem?.type).toBe(
      "https://errors.workforce-management.warehouse-systems.dev/certification-required",
    );
  });

  it("falls back to the status line when the error body is not JSON", async () => {
    stubFetch(
      () =>
        new Response("gateway exploded", {
          status: 502,
          statusText: "Bad Gateway",
        }),
    );

    const err = await apiPost(
      "/associates/assoc-1/assignments",
      { pathId: "pack" },
      "key-4",
    ).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(502);
    expect((err as ApiError).message).toBe("502 Bad Gateway");
  });

  it("returns the parsed JSON body on a 201 (assignLabor)", async () => {
    stubFetch(
      () =>
        new Response(
          JSON.stringify({ associateId: "assoc-1", activePathId: "pack", active: true }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
    );

    const result = await apiPost<{ associateId: string; activePathId: string; active: boolean }>(
      "/associates/assoc-1/assignments",
      { pathId: "pack" },
      "key-5",
    );

    expect(result).toEqual({ associateId: "assoc-1", activePathId: "pack", active: true });
  });
});

describe("newIdempotencyKey", () => {
  it("returns a distinct RFC 4122 v4 UUID on every call", () => {
    const a = newIdempotencyKey();
    const b = newIdempotencyKey();
    expect(a).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
    expect(b).not.toBe(a);
  });
});
