import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";
import { IssueSourceBadge } from "./issue-source-badge";

const mockResolve = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());
const sourceState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: sourceState.current, isError: false }),
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/github", () => ({
  issueExternalSourceOptions: () => ({ queryKey: ["issue-external-source", "issue-1"] }),
  useResolveIssueExternalConflict: () => ({ mutateAsync: (b: unknown) => mockResolve(b) }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

function baseSource(over: Record<string, unknown> = {}) {
  return {
    provider: "github",
    display_number: 7,
    external_url: "https://github.com/acme/widgets/issues/7",
    title_conflict: false,
    body_conflict: false,
    title_local_owned: false,
    body_local_owned: false,
    ...over,
  };
}

function renderBadge() {
  return render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, issues: enIssues } }}>
      <IssueSourceBadge issueId="issue-1" />
    </I18nProvider>,
  );
}

describe("IssueSourceBadge per-field resolution", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sourceState.current = baseSource();
  });

  // A title-only conflict must submit resolution scoped to title, so a local-only
  // body edit is never clobbered. Regression for codex56 round-4 P0-2.
  it("resolves only the conflicting field", async () => {
    const user = userEvent.setup();
    sourceState.current = baseSource({ title_conflict: true });
    renderBadge();

    // Only the title conflict row is shown; no body row.
    expect(screen.getByText("Title sync conflict")).toBeInTheDocument();
    expect(screen.queryByText("Description sync conflict")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Use remote" }));
    await waitFor(() => expect(mockResolve).toHaveBeenCalled());
    expect(mockResolve).toHaveBeenCalledWith({
      action: "use_remote",
      fields: ["title"],
    });
  });

  // After keep_local the field is locally-owned (no conflict): the badge must
  // still offer a per-field Resume so sync can be re-enabled. Regression P1-1.
  it("offers Resume for a locally-owned field", async () => {
    const user = userEvent.setup();
    sourceState.current = baseSource({ body_local_owned: true });
    renderBadge();

    expect(screen.getByText("Description kept local (sync paused)")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Resume sync" }));
    await waitFor(() => expect(mockResolve).toHaveBeenCalled());
    expect(mockResolve).toHaveBeenCalledWith({
      action: "resume_sync",
      fields: ["body"],
    });
  });

  // No conflict and nothing locally-owned → just the provenance badge, no actions.
  it("shows no action rows when clean", () => {
    renderBadge();
    expect(screen.getByText("Imported from github #7")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Use remote" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resume sync" })).not.toBeInTheDocument();
  });
});
