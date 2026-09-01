// @vitest-environment node
import { describe, it, expect, vi, beforeEach } from "vitest";

const mockGetRun = vi.hoisted(() => vi.fn());
vi.mock("../api", () => ({
  api: { getExternalIssueSyncRun: (...a: unknown[]) => mockGetRun(...a) },
}));

import { createQueryClient } from "../query-client";
import { externalIssueSyncRunOptions } from "./mutations";

describe("externalIssueSyncRunOptions polling", () => {
  beforeEach(() => vi.clearAllMocks());

  // The app's global staleTime is Infinity; the polling options MUST override it
  // to staleTime:0 so successive qc.fetchQuery calls actually re-hit the network
  // and observe queued -> succeeded. Regression for codex56 round-7 (frozen poll).
  it("refetches each tick instead of returning the first cached snapshot", async () => {
    mockGetRun
      .mockResolvedValueOnce({ run_id: "run-1", state: "queued" })
      .mockResolvedValueOnce({ run_id: "run-1", state: "succeeded" });

    const qc = createQueryClient();
    const first = await qc.fetchQuery(externalIssueSyncRunOptions("ws-1", "run-1"));
    const second = await qc.fetchQuery(externalIssueSyncRunOptions("ws-1", "run-1"));

    expect(first.state).toBe("queued");
    expect(second.state).toBe("succeeded");
    expect(mockGetRun).toHaveBeenCalledTimes(2);
  });
});
