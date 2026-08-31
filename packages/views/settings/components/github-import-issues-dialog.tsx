"use client";

import { useEffect, useState } from "react";
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

// States a user can resume from (re-queues the SAME run from its saved cursor).
const RESUMABLE_STATES = new Set(["quota_blocked", "needs_reauth", "failed"]);

// Cap for the empty/garbage-state poll: if the server keeps returning an empty
// state (drift), stop after this many ticks instead of polling forever.
const MAX_EMPTY_POLLS = 20;

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
  const [resuming, setResuming] = useState(false);

  const parsed = splitOwnerRepo(fullPath);
  const isRunning = runId !== null && !(status && TERMINAL_STATES.has(status.state));
  const canSubmit = parsed !== null && !starting && !isRunning;
  const canResume = status !== null && RESUMABLE_STATES.has(status.state) && !resuming;

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
  // Serial (no setInterval): each tick is scheduled only after the previous
  // response returns, so a slow response can't be overtaken by a newer one and
  // stale data can't clobber a terminal state.
  useEffect(() => {
    if (!runId) return;
    let stopped = false;
    let emptyPolls = 0;
    let timer: ReturnType<typeof setTimeout> | null = null;
    async function tick() {
      try {
        const s = await api.getExternalIssueSyncRun(workspaceId, runId!);
        if (stopped) return;
        setStatus(s);
        if (s.state && TERMINAL_STATES.has(s.state)) {
          stopped = true;
          void qc.invalidateQueries({ queryKey: issueKeys.all(workspaceId) });
          void qc.invalidateQueries({ queryKey: githubKeys.all(workspaceId) });
          return;
        }
        // A persistently empty state means backend drift, not progress: stop
        // after a bounded number of ticks instead of polling forever.
        if (!s.state) {
          emptyPolls++;
          if (emptyPolls >= MAX_EMPTY_POLLS) {
            stopped = true;
            return;
          }
        } else {
          emptyPolls = 0;
        }
      } catch {
        // Transient poll failure: fall through and retry on the next tick.
      }
      if (!stopped) timer = setTimeout(tick, 1500);
    }
    void tick();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, workspaceId]);

  async function handleResume() {
    if (!runId) return;
    setResuming(true);
    try {
      const res = await api.resumeExternalIssueSyncRun(workspaceId, runId);
      if (!res.run_id) {
        toast.error(t(($) => $.github.import_issues.toast_failed));
        return;
      }
      // Resume returns the same run id; reset status so the poll loop restarts
      // and the UI shows progress again.
      setStatus(null);
      setRunId(res.run_id);
      toast.success(t(($) => $.github.import_issues.toast_resumed));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github.import_issues.toast_failed));
    } finally {
      setResuming(false);
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
      // Strict: a drift/garbage response with no run id must not leave the UI
      // stuck "running" with nothing to poll or cancel.
      if (!res.run_id) {
        toast.error(t(($) => $.github.import_issues.toast_failed));
        return;
      }
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
          {canResume ? (
            <Button onClick={handleResume} disabled={resuming}>
              {resuming
                ? t(($) => $.github.import_issues.resuming)
                : t(($) => $.github.import_issues.resume)}
            </Button>
          ) : (
            <Button onClick={handleStart} disabled={!canSubmit}>
              {t(($) => $.github.import_issues.submit)}
            </Button>
          )}
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
