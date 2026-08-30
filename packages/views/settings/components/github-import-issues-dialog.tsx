"use client";

import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { DownloadCloud } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { api } from "@multica/core/api";
import { githubKeys } from "@multica/core/github";
import { issueKeys } from "@multica/core/issues/queries";
import type { SyncRunStatus } from "@multica/core/types";
import { useT } from "../../i18n";

type ImportState = "open" | "closed" | "all";

const TERMINAL_STATES = new Set([
  "succeeded",
  "partial",
  "failed",
  "cancelled",
  "quota_blocked",
  "needs_reauth",
]);

interface GitHubImportIssuesDialogProps {
  workspaceId: string;
  installationId: string;
  defaultProjectId?: string;
  defaultFullPath?: string;
}

/**
 * GitHubImportIssuesDialog kicks off an async import (202 + run id) and polls the
 * run's progress until it reaches a terminal state, showing live
 * imported/updated/conflict/failed counts with a cancel control. It never blocks
 * on a single request draining every page.
 */
export function GitHubImportIssuesDialog({
  workspaceId,
  installationId,
  defaultProjectId,
  defaultFullPath,
}: GitHubImportIssuesDialogProps) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [fullPath, setFullPath] = useState(defaultFullPath ?? "");
  const [state, setState] = useState<ImportState>("open");
  const [runId, setRunId] = useState<string | null>(null);
  const [status, setStatus] = useState<SyncRunStatus | null>(null);
  const [starting, setStarting] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const parsed = splitOwnerRepo(fullPath);
  const isRunning = runId !== null && !(status && TERMINAL_STATES.has(status.state));
  const canSubmit = parsed !== null && !starting && !isRunning;

  // Localized label for a run state. Unknown/in-flight states fall back to the
  // running label (server-driven enum: always has a default branch).
  function runStateLabel(runState: string): string {
    switch (runState) {
      case "queued":
        return t(($) => $.github.import_issues.state_queued);
      case "succeeded":
        return t(($) => $.github.import_issues.state_succeeded);
      case "partial":
        return t(($) => $.github.import_issues.state_partial);
      case "failed":
        return t(($) => $.github.import_issues.state_failed);
      case "cancelled":
        return t(($) => $.github.import_issues.state_cancelled);
      case "quota_blocked":
        return t(($) => $.github.import_issues.state_quota_blocked);
      case "needs_reauth":
        return t(($) => $.github.import_issues.state_needs_reauth);
      default:
        return t(($) => $.github.import_issues.state_running);
    }
  }

  // Poll the active run until it reaches a terminal state, then refresh caches.
  useEffect(() => {
    if (!runId) return;
    let cancelled = false;
    async function poll() {
      try {
        const s = await api.getExternalIssueSyncRun(workspaceId, runId!);
        if (cancelled) return;
        setStatus(s);
        if (TERMINAL_STATES.has(s.state)) {
          stopPolling();
          void qc.invalidateQueries({ queryKey: issueKeys.all(workspaceId) });
          void qc.invalidateQueries({ queryKey: githubKeys.all(workspaceId) });
        }
      } catch {
        // Transient poll failure: keep the interval; the next tick retries.
      }
    }
    void poll();
    pollRef.current = setInterval(poll, 1500);
    return () => {
      cancelled = true;
      stopPolling();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, workspaceId]);

  function stopPolling() {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }

  async function handleStart() {
    if (!parsed) return;
    setStarting(true);
    setStatus(null);
    try {
      const res = await api.importGitHubIssues(workspaceId, installationId, {
        owner: parsed.owner,
        repo: parsed.repo,
        state,
        project_id: defaultProjectId,
      });
      setRunId(res.run_id);
      toast.success(t(($) => $.github.import_issues.toast_queued));
    } catch (e) {
      const message = e instanceof Error ? e.message : "";
      if (message.toLowerCase().includes("issues")) {
        toast.error(t(($) => $.github.import_issues.toast_permission));
      } else {
        toast.error(message || t(($) => $.github.import_issues.toast_failed));
      }
    } finally {
      setStarting(false);
    }
  }

  async function handleCancel() {
    if (!runId) return;
    setCancelling(true);
    try {
      await api.cancelExternalIssueSyncRun(workspaceId, runId);
      toast.success(t(($) => $.github.import_issues.toast_cancel_requested));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github.import_issues.toast_failed));
    } finally {
      setCancelling(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <DownloadCloud className="size-4" />
            {t(($) => $.github.import_issues.trigger)}
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t(($) => $.github.import_issues.title)}</DialogTitle>
          <DialogDescription>
            {t(($) => $.github.import_issues.description)}
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="gh-import-repo">
              {t(($) => $.github.import_issues.repo_label)}
            </Label>
            <Input
              id="gh-import-repo"
              placeholder={t(($) => $.github.import_issues.repo_placeholder)}
              value={fullPath}
              onChange={(e) => setFullPath(e.target.value)}
              disabled={isRunning}
              autoComplete="off"
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <Label htmlFor="gh-import-state">
              {t(($) => $.github.import_issues.state_label)}
            </Label>
            <select
              id="gh-import-state"
              className="border-input bg-background h-9 rounded-md border px-3 text-body"
              value={state}
              onChange={(e) => setState(e.target.value as ImportState)}
              disabled={isRunning}
            >
              <option value="open">{t(($) => $.github.import_issues.state_open)}</option>
              <option value="closed">{t(($) => $.github.import_issues.state_closed)}</option>
              <option value="all">{t(($) => $.github.import_issues.state_all)}</option>
            </select>
          </div>

          {status && (
            <div className="flex flex-col gap-1">
              <span className="text-body font-medium">{runStateLabel(status.state)}</span>
              <span className="text-caption text-muted-foreground">
                {t(($) => $.github.import_issues.summary, {
                  imported: status.imported,
                  updated: status.updated,
                  conflicts: status.conflicts,
                  skipped: status.skipped,
                  failed: status.failed,
                })}
              </span>
            </div>
          )}
        </div>

        <DialogFooter>
          {isRunning ? (
            <Button variant="outline" onClick={handleCancel} disabled={cancelling}>
              {cancelling
                ? t(($) => $.github.import_issues.cancelling)
                : t(($) => $.github.import_issues.cancel)}
            </Button>
          ) : (
            <Button variant="outline" onClick={() => setOpen(false)}>
              {t(($) => $.github.import_issues.close)}
            </Button>
          )}
          <Button onClick={handleStart} disabled={!canSubmit}>
            {t(($) => $.github.import_issues.submit)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** splitOwnerRepo parses "owner/repo" (tolerating surrounding whitespace and a
 * trailing slash), returning null when the shape is invalid so the submit
 * button can stay disabled. */
export function splitOwnerRepo(input: string): { owner: string; repo: string } | null {
  const trimmed = input.trim().replace(/\/+$/, "");
  const parts = trimmed.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) return null;
  return { owner: parts[0], repo: parts[1] };
}
