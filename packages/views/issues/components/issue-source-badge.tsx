"use client";

import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink, GitMerge } from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  issueExternalSourceOptions,
  useResolveIssueExternalConflict,
} from "@multica/core/github";
import { useT } from "../../i18n";

interface IssueSourceBadgeProps {
  wsId: string;
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
export function IssueSourceBadge({ wsId, issueId }: IssueSourceBadgeProps) {
  const { t } = useT("issues");
  const { data: source, isError } = useQuery(issueExternalSourceOptions(issueId));
  const resolveMutation = useResolveIssueExternalConflict(wsId, issueId);

  // No source (404) or not loaded yet → render nothing.
  if (isError || !source || !source.provider) return null;

  // Per-field state: a field is actionable when it is in conflict OR the user
  // previously chose "keep local" (locally-owned), which needs a Resume entry so
  // sync can be re-enabled later. Resolution is submitted per field so acting on
  // one field never touches the other's content or ownership.
  const titleActionable = source.title_conflict === true || source.title_local_owned === true;
  const bodyActionable = source.body_conflict === true || source.body_local_owned === true;

  async function resolve(
    action: "keep_local" | "use_remote" | "resume_sync",
    field: "title" | "body",
  ) {
    try {
      await resolveMutation.mutateAsync({ action, fields: [field] });
      toast.success(t(($) => $.external_source.toast_resolved));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.external_source.toast_failed));
    }
  }

  function fieldRow(
    field: "title" | "body",
    conflict: boolean,
    localOwned: boolean,
    label: string,
  ) {
    if (conflict) {
      return (
        <div key={field} className="flex flex-wrap items-center gap-2">
          <span className="text-caption text-destructive">
            {t(($) => $.external_source.field_conflict, { field: label })}
          </span>
          <Button variant="outline" size="sm" onClick={() => resolve("keep_local", field)}>
            {t(($) => $.external_source.keep_local)}
          </Button>
          <Button variant="outline" size="sm" onClick={() => resolve("use_remote", field)}>
            {t(($) => $.external_source.use_remote)}
          </Button>
        </div>
      );
    }
    // Locally-owned (kept local earlier), no active conflict: offer Resume so the
    // user can re-enable remote sync for this field.
    if (localOwned) {
      return (
        <div key={field} className="flex flex-wrap items-center gap-2">
          <span className="text-caption text-muted-foreground">
            {t(($) => $.external_source.field_local_owned, { field: label })}
          </span>
          <Button variant="ghost" size="sm" onClick={() => resolve("resume_sync", field)}>
            {t(($) => $.external_source.resume_sync)}
          </Button>
        </div>
      );
    }
    return null;
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

      {titleActionable
        ? fieldRow(
            "title",
            source.title_conflict === true,
            source.title_local_owned === true,
            t(($) => $.external_source.field_title),
          )
        : null}
      {bodyActionable
        ? fieldRow(
            "body",
            source.body_conflict === true,
            source.body_local_owned === true,
            t(($) => $.external_source.field_body),
          )
        : null}
    </div>
  );
}
