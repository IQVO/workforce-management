import { describe, expect, it } from "vitest";
import { allPathsStaffingGapUrl, singlePathStaffingGapUrl } from "./apiUrls";

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

describe("singlePathStaffingGapUrl", () => {
  it("keeps the one-path lookup shape with both query params", () => {
    expect(singlePathStaffingGapUrl("http://localhost:8085", "pack", "bldg-1", "shift-1")).toBe(
      "http://localhost:8085/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1",
    );
  });

  it("URI-encodes the path segment and query values", () => {
    expect(singlePathStaffingGapUrl("http://x", "pick zone", "b 1", "s 1")).toBe(
      "http://x/paths/pick%20zone/staffing-gap?buildingId=b%201&shiftId=s%201",
    );
  });
});
