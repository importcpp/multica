"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink, GitMerge } from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import { issueExternalSourceOptions } from "@multica/core/github";
import { useT } from "../../i18n";

interface IssueSourceBadgeProps {
  issueId: string;
}

/**
 * IssueSourceBadge renders the provenance of an imported issue — "Imported from
 * <provider>", a link to the remote issue, and (when the sync detected a
 * divergence) a conflict row with keep-local / use-remote / resume actions. The
 * remote body is kept verbatim on the issue; this badge is how provenance is
 * surfaced instead of appending "Imported from ..." into the description.
 * Renders nothing when the issue was not imported (404 → no source).
 */
export function IssueSourceBadge({ issueId }: IssueSourceBadgeProps) {
  const { t } = useT("issues");
  const qc = useQueryClient();
  const { data: source, isError } = useQuery(issueExternalSourceOptions(issueId));

  // No source (404) or not loaded yet → render nothing.
  if (isError || !source || !source.provider) return null;

  const hasConflict = source.title_conflict === true || source.body_conflict === true;

  async function resolve(action: "keep_local" | "use_remote" | "resume_sync") {
    try {
      await api.resolveIssueExternalConflict(issueId, { action });
      await qc.invalidateQueries({ queryKey: ["issue-external-source", issueId] });
      toast.success(t(($) => $.external_source.toast_resolved));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.external_source.toast_failed));
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <Badge variant="secondary" className="gap-1">
          <GitMerge className="size-3" />
          {t(($) => $.external_source.label, { provider: source.provider })}
          {source.display_number > 0 ? ` #${source.display_number}` : ""}
        </Badge>
        {source.external_url ? (
          <a
            href={source.external_url}
            target="_blank"
            rel="noreferrer"
            className="text-caption text-muted-foreground inline-flex items-center gap-1 hover:text-foreground"
          >
            <ExternalLink className="size-3" />
            {t(($) => $.external_source.view_remote, { provider: source.provider })}
          </a>
        ) : null}
      </div>

      {hasConflict ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-caption text-destructive">
            {t(($) => $.external_source.conflict)}
          </span>
          <Button variant="outline" size="sm" onClick={() => resolve("keep_local")}>
            {t(($) => $.external_source.keep_local)}
          </Button>
          <Button variant="outline" size="sm" onClick={() => resolve("use_remote")}>
            {t(($) => $.external_source.use_remote)}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => resolve("resume_sync")}>
            {t(($) => $.external_source.resume_sync)}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
