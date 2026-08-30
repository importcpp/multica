// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  ImportGitHubIssuesResponseSchema,
  EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
  SyncRunStatusSchema,
  EMPTY_SYNC_RUN_STATUS,
} from "@multica/core/api/schemas";
import { parseWithFallback } from "@multica/core/api/schema";
import { splitOwnerRepo } from "./github-import-issues-dialog";

describe("splitOwnerRepo", () => {
  it("parses a well-formed owner/repo", () => {
    expect(splitOwnerRepo("multica-ai/multica")).toEqual({
      owner: "multica-ai",
      repo: "multica",
    });
  });

  it("tolerates whitespace and a trailing slash", () => {
    expect(splitOwnerRepo("  multica-ai/multica/  ")).toEqual({
      owner: "multica-ai",
      repo: "multica",
    });
  });

  it("rejects invalid shapes", () => {
    for (const bad of ["", "multica", "a/b/c", "/x", "x/", "  "]) {
      expect(splitOwnerRepo(bad)).toBeNull();
    }
  });
});

describe("ImportGitHubIssuesResponse schema", () => {
  it("parses the 202 enqueue response", () => {
    const parsed = parseWithFallback(
      { source_id: "s1", run_id: "r1", state: "queued" },
      ImportGitHubIssuesResponseSchema,
      EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
      { endpoint: "test" },
    );
    expect(parsed.run_id).toBe("r1");
    expect(parsed.state).toBe("queued");
  });

  it("falls back on a malformed response (API drift)", () => {
    const parsed = parseWithFallback(
      { run_id: 123, nonsense: true },
      ImportGitHubIssuesResponseSchema,
      EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
      { endpoint: "test" },
    );
    expect(parsed).toEqual(EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE);
  });
});

describe("SyncRunStatus schema", () => {
  it("parses a running status with counts", () => {
    const parsed = parseWithFallback(
      {
        run_id: "r1",
        source_id: "s1",
        state: "running",
        imported: 12,
        updated: 3,
        conflicts: 1,
        skipped: 0,
        failed: 2,
        total: 18,
        cancel_requested: false,
      },
      SyncRunStatusSchema,
      EMPTY_SYNC_RUN_STATUS,
      { endpoint: "test" },
    );
    expect(parsed.state).toBe("running");
    expect(parsed.imported).toBe(12);
    expect(parsed.failed).toBe(2);
  });

  it("falls back to an empty status on garbage (never throws in the poll loop)", () => {
    const parsed = parseWithFallback(
      { state: 42, imported: "lots" },
      SyncRunStatusSchema,
      EMPTY_SYNC_RUN_STATUS,
      { endpoint: "test" },
    );
    expect(parsed).toEqual(EMPTY_SYNC_RUN_STATUS);
  });

  it("defaults missing optional fields", () => {
    const parsed = parseWithFallback(
      { run_id: "r1", state: "succeeded" },
      SyncRunStatusSchema,
      EMPTY_SYNC_RUN_STATUS,
      { endpoint: "test" },
    );
    expect(parsed.state).toBe("succeeded");
    expect(parsed.imported).toBe(0);
    expect(parsed.cancel_requested).toBe(false);
  });
});
