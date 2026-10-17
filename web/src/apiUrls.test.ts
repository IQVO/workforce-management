import { describe, expect, it } from "vitest";
import {
  allPathsStaffingGapUrl,
  assignLaborUrl,
  certifyAssociateUrl,
  singlePathStaffingGapUrl,
  siteShiftStaffingGapUrl,
} from "./apiUrls";

describe("allPathsStaffingGapUrl", () => {
  it("builds the list-all-paths endpoint for a building/shift", () => {
    expect(allPathsStaffingGapUrl("http://localhost:8085", "bldg-1", "shift-1")).toBe(
      "http://localhost:8085/buildings/bldg-1/shifts/shift-1/staffing-gap",
    );
  });

  it("URI-encodes path segments", () => {
    expect(allPathsStaffingGapUrl("http://x", "bldg 1", "shift/1")).toBe(
      "http://x/buildings/bldg%201/shifts/shift%2F1/staffing-gap",
    );
  });
});

describe("siteShiftStaffingGapUrl", () => {
  it("builds the canonical per-shift endpoint with the site code (ADR 0035)", () => {
    expect(siteShiftStaffingGapUrl("http://localhost:8085", "WH1", "shift-1")).toBe(
      "http://localhost:8085/sites/WH1/shifts/shift-1/staffing-gap",
    );
  });

  it("URI-encodes path segments", () => {
    expect(siteShiftStaffingGapUrl("http://x", "WH 1", "shift/1")).toBe(
      "http://x/sites/WH%201/shifts/shift%2F1/staffing-gap",
    );
  });
});

describe("singlePathStaffingGapUrl", () => {
  it("keeps the one-path lookup shape with the canonical siteCode and shiftId", () => {
    expect(singlePathStaffingGapUrl("http://localhost:8085", "pack", "WH1", "shift-1")).toBe(
      "http://localhost:8085/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1",
    );
  });

  it("URI-encodes the path segment and query values", () => {
    expect(singlePathStaffingGapUrl("http://x", "pick zone", "W 1", "s 1")).toBe(
      "http://x/paths/pick%20zone/staffing-gap?siteCode=W%201&shiftId=s%201",
    );
  });
});

describe("certifyAssociateUrl", () => {
  it("builds POST /associates/{id}/certifications", () => {
    expect(certifyAssociateUrl("http://localhost:8085", "assoc-1")).toBe(
      "http://localhost:8085/associates/assoc-1/certifications",
    );
  });

  it("URI-encodes the associate id", () => {
    expect(certifyAssociateUrl("http://x", "assoc 1")).toBe(
      "http://x/associates/assoc%201/certifications",
    );
  });
});

describe("assignLaborUrl", () => {
  it("builds POST /associates/{id}/assignments", () => {
    expect(assignLaborUrl("http://localhost:8085", "assoc-1")).toBe(
      "http://localhost:8085/associates/assoc-1/assignments",
    );
  });

  it("URI-encodes the associate id", () => {
    expect(assignLaborUrl("http://x", "assoc/1")).toBe(
      "http://x/associates/assoc%2F1/assignments",
    );
  });
});
