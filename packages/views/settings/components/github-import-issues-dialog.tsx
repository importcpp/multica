"use client";

import { useState } from "react";
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
import type { ImportGitHubIssuesResponse } from "@multica/core/types";
import { useT } from "../../i18n";

type ImportState = "open" | "closed" | "all";

interface GitHubImportIssuesDialogProps {
  workspaceId: string;
  installationId: string;
  /** Optional default target project for the imported issues. */
  defaultProjectId?: string;
  /** Optional pre-filled "owner/repo" from the repository picker. */
  defaultFullPath?: string;
}

/**
 * GitHubImportIssuesDialog is the admin-facing entry point for pulling GitHub
 * issues into the workspace. It posts to the import endpoint and shows a
 * created/updated/conflict/failed summary. Backfill is bounded server-side, so
 * a `truncated` result tells the user to run it again to continue.
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
  const [importing, setImporting] = useState(false);
  const [result, setResult] = useState<ImportGitHubIssuesResponse | null>(null);

  const parsed = splitOwnerRepo(fullPath);
  const canSubmit = parsed !== null && !importing;

  async function handleImport() {
    if (!parsed) return;
    setImporting(true);
    setResult(null);
    try {
      const res = await api.importGitHubIssues(workspaceId, installationId, {
        owner: parsed.owner,
        repo: parsed.repo,
        state,
        project_id: defaultProjectId,
      });
      setResult(res);
      // Refetch issue lists so the imported issues appear without a manual
      // reload, and the github views if they surface source state.
      void qc.invalidateQueries({ queryKey: issueKeys.all(workspaceId) });
      void qc.invalidateQueries({ queryKey: githubKeys.all(workspaceId) });
      const summary = t(($) => $.github.import_issues.summary, {
        imported: res.imported,
        updated: res.updated,
        conflicts: res.conflicts,
        skipped: res.skipped,
        failed: res.failed,
      });
      if (res.truncated) {
        toast.warning(t(($) => $.github.import_issues.toast_truncated, { summary }));
      } else {
        toast.success(t(($) => $.github.import_issues.toast_complete, { summary }));
      }
    } catch (e) {
      const message = e instanceof Error ? e.message : "";
      // A missing Issues permission is the common actionable case.
      if (message.toLowerCase().includes("issues")) {
        toast.error(t(($) => $.github.import_issues.toast_permission));
      } else {
        toast.error(message || t(($) => $.github.import_issues.toast_failed));
      }
    } finally {
      setImporting(false);
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
            >
              <option value="open">{t(($) => $.github.import_issues.state_open)}</option>
              <option value="closed">{t(($) => $.github.import_issues.state_closed)}</option>
              <option value="all">{t(($) => $.github.import_issues.state_all)}</option>
            </select>
          </div>

          {result && (
            <div className="text-caption text-muted-foreground flex flex-col gap-1">
              <span>
                {t(($) => $.github.import_issues.summary, {
                  imported: result.imported,
                  updated: result.updated,
                  conflicts: result.conflicts,
                  skipped: result.skipped,
                  failed: result.failed,
                })}
              </span>
              {result.truncated && (
                <span>{t(($) => $.github.import_issues.truncated_note)}</span>
              )}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)} disabled={importing}>
            {t(($) => $.github.import_issues.close)}
          </Button>
          <Button onClick={handleImport} disabled={!canSubmit}>
            {importing
              ? t(($) => $.github.import_issues.submitting)
              : t(($) => $.github.import_issues.submit)}
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
