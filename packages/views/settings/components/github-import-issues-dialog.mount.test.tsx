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
const mockPreview = vi.hoisted(() => vi.fn());
const mockCancel = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());
// Repos surfaced by the installation picker; empty by default so tests that
// don't care fall back to the manual owner/repo <Input>.
const repoState = vi.hoisted(() => ({ repositories: [] as { id: number; full_name: string }[] }));

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({
    invalidateQueries: mockInvalidate,
    // The dialog polls via qc.fetchQuery(externalIssueSyncRunOptions(...)); the
    // options carry the queryFn, so invoke it to reach the mocked getRun.
    fetchQuery: (opts: { queryFn: () => unknown }) => opts.queryFn(),
  }),
  queryOptions: <T,>(opts: T) => opts,
  infiniteQueryOptions: <T,>(opts: T) => opts,
  useInfiniteQuery: () => ({
    data: { pages: [{ repositories: repoState.repositories }] },
    isLoading: false,
    hasNextPage: false,
    isFetchingNextPage: false,
    fetchNextPage: vi.fn(),
  }),
}));

vi.mock("@multica/core/github", () => ({
  githubKeys: { all: () => ["github"] },
  githubInstallationRepositoriesOptions: () => ({ queryKey: ["repos"] }),
  externalIssueSyncRunOptions: (wsId: string, runId: string) => ({
    queryKey: ["run", wsId, runId],
    queryFn: () => mockGetRun(wsId, runId),
  }),
  usePreviewGitHubIssues: () => ({ mutateAsync: (b: unknown) => mockPreview(b) }),
  useImportGitHubIssues: () => ({ mutateAsync: (b: unknown) => mockImport(b) }),
  useResumeExternalIssueSyncRun: () => ({ mutateAsync: (id: unknown) => mockResume(id) }),
  useCancelExternalIssueSyncRun: () => ({ mutateAsync: (id: unknown) => mockCancel(id) }),
}));
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

function previewResponse(over: Record<string, unknown> = {}) {
  return {
    sample: [{ number: 1, title: "first", state: "open" }],
    sample_count: 1,
    has_more: false,
    capacity_remaining: -1,
    capacity_limited: false,
    ...over,
  };
}

// Import is gated behind a fresh preview: open the dialog, preview, then click
// Import. Mirrors the real user flow the gate enforces.
async function openPreviewAndImport(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /import issues/i }));
  await user.click(screen.getByRole("button", { name: "Preview" }));
  const importBtn = await screen.findByRole("button", { name: "Import" });
  await waitFor(() => expect(importBtn).toBeEnabled());
  await user.click(importBtn);
}

describe("GitHubImportIssuesDialog resume poll", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    repoState.repositories = [];
  });

  // A quota_blocked run shows Resume; clicking it must restart polling even
  // though the run id is unchanged, and the UI must advance to succeeded.
  // Regression for codex56 P1-5 (same-run-id resume didn't re-run the effect).
  it("restarts polling on resume with the same run id", async () => {
    const user = userEvent.setup();
    mockPreview.mockResolvedValue(previewResponse());
    mockImport.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockResume.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockGetRun
      .mockResolvedValueOnce(runStatus("quota_blocked", { imported: 1, total: 1 }))
      .mockResolvedValue(runStatus("succeeded", { imported: 2, total: 2 }));

    renderDialog();
    await openPreviewAndImport(user);

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
    mockPreview.mockResolvedValue(previewResponse());
    mockImport.mockResolvedValue({ source_id: "src-1", run_id: "run-1", state: "queued" });
    mockGetRun.mockResolvedValue(
      runStatus("partial", { imported: 1, failed: 1, total: 2, errors: ["issue #7: boom"] }),
    );
    renderDialog();
    await openPreviewAndImport(user);
    await waitFor(() => expect(screen.getByText("issue #7: boom")).toBeInTheDocument());
  });

  // Import stays disabled until a fresh preview exists, and a preview goes stale
  // the moment the repo/state inputs change. Regression for codex56 preview gate.
  it("gates Import on a fresh preview and clears it on input change", async () => {
    const user = userEvent.setup();
    mockPreview.mockResolvedValue(previewResponse());
    renderDialog();
    await user.click(screen.getByRole("button", { name: /import issues/i }));

    // No preview yet → Import disabled.
    expect(screen.getByRole("button", { name: "Import" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Import" })).toBeEnabled());

    // Changing the repo invalidates the preview → Import disabled again.
    const repoInput = screen.getByLabelText(/repository/i);
    await user.type(repoInput, "-2");
    await waitFor(() => expect(screen.getByRole("button", { name: "Import" })).toBeDisabled());
    expect(mockImport).not.toHaveBeenCalled();
  });

  // When the installation picker has repos, the user selects one from the
  // dropdown instead of typing owner/repo, and preview uses that selection.
  it("selects a repository from the installation picker", async () => {
    repoState.repositories = [
      { id: 1, full_name: "acme/widgets" },
      { id: 2, full_name: "acme/gadgets" },
    ];
    mockPreview.mockResolvedValue(previewResponse());
    const user = userEvent.setup();
    // Render WITHOUT a defaultFullPath so the picker drives selection.
    render(
      <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
        <GitHubImportIssuesDialog workspaceId="ws-1" installationId="inst-1" pollIntervalMs={5} />
      </I18nProvider>,
    );
    await user.click(screen.getByRole("button", { name: /import issues/i }));

    const select = screen.getByLabelText(/repository/i);
    await user.selectOptions(select, "acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Preview" }));

    await waitFor(() => expect(mockPreview).toHaveBeenCalled());
    // The hook is bound to ws+installation; mutateAsync receives just the body.
    expect(mockPreview).toHaveBeenCalledWith(
      expect.objectContaining({ owner: "acme", repo: "gadgets" }),
    );
  });
});
