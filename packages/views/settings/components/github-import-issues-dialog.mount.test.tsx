import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import { GitHubImportIssuesDialog } from "./github-import-issues-dialog";

const mockImport = vi.hoisted(() => vi.fn());
const mockResume = vi.hoisted(() => vi.fn());
const mockGetRun = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/api", () => ({
  api: {
    importGitHubIssues: (...a: unknown[]) => mockImport(...a),
    resumeExternalIssueSyncRun: (...a: unknown[]) => mockResume(...a),
    getExternalIssueSyncRun: (...a: unknown[]) => mockGetRun(...a),
    previewGitHubIssues: vi.fn(),
    cancelExternalIssueSyncRun: vi.fn(),
  },
}));

vi.mock("@multica/core/github", () => ({ githubKeys: { all: () => ["github"] } }));
vi.mock("@multica/core/issues/queries", () => ({ issueKeys: { all: () => ["issues"] } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

function renderDialog() {
  return render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
      <GitHubImportIssuesDialog
        workspaceId="ws-1"
        installationId="inst-1"
        defaultFullPath="acme/widgets"
        pollIntervalMs={5}
      />
    </I18nProvider>,
  );
}

function runStatus(state: string, over: Record<string, unknown> = {}) {
  return {
    run_id: "run-1", source_id: "src-1", state,
    imported: 0, updated: 0, conflicts: 0, skipped: 0, failed: 0, total: 0,
    cancel_requested: false, errors: [], ...over,
  };
}

describe("GitHubImportIssuesDialog resume poll", () => {
  beforeEach(() => vi.clearAllMocks());

  // A quota_blocked run shows Resume; clicking it must restart polling even
  // though the run id is unchanged, and the UI must advance to succeeded.
  // Regression for codex56 P1-5 (same-run-id resume didn't re-run the effect).
  it("restarts polling on resume with the same run id", async () => {
    const user = userEvent.setup();
    mockImport.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockResume.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockGetRun
      .mockResolvedValueOnce(runStatus("quota_blocked", { imported: 1, total: 1 }))
      .mockResolvedValue(runStatus("succeeded", { imported: 2, total: 2 }));

    renderDialog();
    await user.click(screen.getByRole("button", { name: /import issues/i }));
    await user.click(screen.getByRole("button", { name: "Import" }));

    // First poll lands on quota_blocked → Resume button appears.
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Resume" }));
    // Polling restarts (same run id) and reaches the succeeded state label.
    await waitFor(() => expect(screen.getByText("Import complete")).toBeInTheDocument());
    expect(mockResume).toHaveBeenCalledOnce();
  });

  // A partial run's per-issue error sample is shown to the user.
  it("shows the failed-issue error sample", async () => {
    const user = userEvent.setup();
    mockImport.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockGetRun.mockResolvedValue(
      runStatus("partial", { imported: 1, failed: 1, total: 2, errors: ["issue #7: boom"] }),
    );
    renderDialog();
    await user.click(screen.getByRole("button", { name: /import issues/i }));
    await user.click(screen.getByRole("button", { name: "Import" }));
    await waitFor(() => expect(screen.getByText("issue #7: boom")).toBeInTheDocument());
  });
});
