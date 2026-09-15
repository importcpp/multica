// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

const bucket = {
  agent_id: "a",
  bucket_at: "2026-09-15T00:00:00Z",
  task_count: 10,
  failed_count: 1,
};

afterEach(() => vi.unstubAllGlobals());

async function read(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 })),
  );
  return new ApiClient("https://api.example.test").getWorkspaceAgentActivity30d();
}

describe("agent activity API outcomes", () => {
  it("preserves explicit outcome counts, including zero", async () => {
    expect(await read([{ ...bucket, completed_count: 0, cancelled_count: 9 }])).toEqual([
      { ...bucket, completed_count: 0, cancelled_count: 9 },
    ]);
  });

  it("preserves older activity without inventing completed or cancelled counts", async () => {
    const [row] = await read([bucket]);
    expect(row).toMatchObject(bucket);
    expect(row?.completed_count).toBeUndefined();
    expect(row?.cancelled_count).toBeUndefined();
  });

  it.each([null, "1", -1, 1.5])("treats malformed outcome %s as unknown", async (bad) => {
    const [row] = await read([{ ...bucket, completed_count: bad, cancelled_count: bad }]);
    expect(row).toMatchObject(bucket);
    expect(row?.completed_count).toBeUndefined();
    expect(row?.cancelled_count).toBeUndefined();
  });

  it("rejects contradictory outcome totals without discarding activity", async () => {
    const [row] = await read([{ ...bucket, completed_count: 10, cancelled_count: 8 }]);
    expect(row).toMatchObject(bucket);
    expect(row?.completed_count).toBeUndefined();
    expect(row?.cancelled_count).toBeUndefined();
  });

  it("falls back for a malformed list", async () => {
    expect(await read({ buckets: [] })).toEqual([]);
  });
});
