// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  ImportGitHubIssuesResponseSchema,
  EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
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
  it("parses a full response", () => {
    const parsed = parseWithFallback(
      {
        source_id: "s1",
        run_id: "r1",
        imported: 3,
        updated: 1,
        conflicts: 2,
        skipped: 0,
        failed: 1,
        total: 7,
        truncated: true,
      },
      ImportGitHubIssuesResponseSchema,
      EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
      { endpoint: "test" },
    );
    expect(parsed.imported).toBe(3);
    expect(parsed.truncated).toBe(true);
  });

  it("falls back to zeros on a malformed response (API drift)", () => {
    const parsed = parseWithFallback(
      { imported: "not-a-number", nonsense: true },
      ImportGitHubIssuesResponseSchema,
      EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
      { endpoint: "test" },
    );
    // A drifting/garbage payload must not throw; it degrades to the empty shape.
    expect(parsed).toEqual(EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE);
  });

  it("defaults missing optional fields", () => {
    const parsed = parseWithFallback(
      { imported: 5 },
      ImportGitHubIssuesResponseSchema,
      EMPTY_IMPORT_GITHUB_ISSUES_RESPONSE,
      { endpoint: "test" },
    );
    expect(parsed.imported).toBe(5);
    expect(parsed.failed).toBe(0);
    expect(parsed.truncated).toBe(false);
  });
});
