import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { githubKeys } from "./queries";
import { issueKeys } from "../issues/queries";
import type {
  ImportGitHubIssuesResponse,
  PreviewGitHubIssuesResponse,
  SyncRunStatus,
} from "../types";

// externalIssueKeys namespaces the import/sync-run server state so views never
// call api.* directly (repo rule: server state flows through TanStack Query).
export const externalIssueKeys = {
  syncRun: (wsId: string, runId: string) =>
    ["github", wsId, "external-issue-sync-run", runId] as const,
  issueSource: (issueId: string) => ["issue-external-source", issueId] as const,
};

// externalIssueSyncRunOptions polls one run's progress. The dialog drives its own
// serial poll cadence (Resume can restart it), so it reads via fetchQuery/the
// query fn rather than a mounted useQuery; this options object is the single
// definition of the key + fetcher.
export const externalIssueSyncRunOptions = (wsId: string, runId: string) =>
  queryOptions({
    queryKey: externalIssueKeys.syncRun(wsId, runId),
    queryFn: () => api.getExternalIssueSyncRun(wsId, runId),
    enabled: !!wsId && !!runId,
  });

export function usePreviewGitHubIssues(wsId: string, installationId: string) {
  return useMutation({
    mutationFn: (body: { owner: string; repo: string; state?: string }) =>
      api.previewGitHubIssues(wsId, installationId, body),
  });
}

export function useImportGitHubIssues(wsId: string, installationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { owner: string; repo: string; state?: string; project_id?: string }) =>
      api.importGitHubIssues(wsId, installationId, body),
    onSuccess: () => {
      // A backfill will create issues out of band; refresh the installations and
      // issue lists so the new run/source and imported issues surface.
      void qc.invalidateQueries({ queryKey: githubKeys.all(wsId) });
      void qc.invalidateQueries({ queryKey: issueKeys.all(wsId) });
    },
  });
}

export function useResumeExternalIssueSyncRun(
  wsId: string,
): ReturnType<typeof useMutation<ImportGitHubIssuesResponse, Error, string>> {
  return useMutation<ImportGitHubIssuesResponse, Error, string>({
    mutationFn: (runId: string) => api.resumeExternalIssueSyncRun(wsId, runId),
  });
}

export function useCancelExternalIssueSyncRun(wsId: string) {
  return useMutation({
    mutationFn: (runId: string) => api.cancelExternalIssueSyncRun(wsId, runId),
  });
}

export function useResolveIssueExternalConflict(issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: {
      action: "keep_local" | "resume_sync" | "use_remote";
      fields?: string[];
    }) => api.resolveIssueExternalConflict(issueId, body),
    onSuccess: () => {
      // The badge reads the source provenance; refresh it plus the issue itself
      // (use_remote overwrites content).
      void qc.invalidateQueries({ queryKey: externalIssueKeys.issueSource(issueId) });
    },
  });
}

export type {
  ImportGitHubIssuesResponse,
  PreviewGitHubIssuesResponse,
  SyncRunStatus,
};
