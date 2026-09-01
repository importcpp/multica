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
// serial poll cadence (Resume can restart it), reading via qc.fetchQuery. It
// MUST set staleTime:0 to override the app's global staleTime:Infinity — otherwise
// fetchQuery returns the first cached snapshot forever and the run state, scanned
// count, cancel and terminal transitions never update. gcTime:0 keeps the cache
// from accumulating one entry per finished run.
export const externalIssueSyncRunOptions = (wsId: string, runId: string) =>
  queryOptions({
    queryKey: externalIssueKeys.syncRun(wsId, runId),
    queryFn: () => api.getExternalIssueSyncRun(wsId, runId),
    enabled: !!wsId && !!runId,
    staleTime: 0,
    gcTime: 0,
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

export function useResolveIssueExternalConflict(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: {
      action: "keep_local" | "resume_sync" | "use_remote";
      fields?: string[];
    }) => api.resolveIssueExternalConflict(issueId, body),
    onSuccess: () => {
      // Refresh the source provenance (badge) AND the issue itself: use_remote
      // overwrites the issue's title/body, and under the app's global
      // staleTime:Infinity the issue detail would otherwise stay the old value
      // until a WS event or manual refetch. Invalidate detail + the list caches.
      void qc.invalidateQueries({ queryKey: externalIssueKeys.issueSource(issueId) });
      void qc.invalidateQueries({ queryKey: issueKeys.detail(wsId, issueId) });
      void qc.invalidateQueries({ queryKey: issueKeys.all(wsId) });
    },
  });
}

export type {
  ImportGitHubIssuesResponse,
  PreviewGitHubIssuesResponse,
  SyncRunStatus,
};
