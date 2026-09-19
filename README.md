# PR #8567 browser evidence

Synthetic reverse-completion transcript rendered by Multica's real AgentTranscriptDialog in local Chromium (1440x1000). No user data or real model calls.

- Before: build-timeline.ts and build-steps.ts from main 2df765a3c8f39789c9fb76316378bcffc20d22d9. A starts at 0s, B at 1s; B completes at 3s and A at 10s. Clicking A incorrectly shows B finished and 3.0s, while B displays 9.0s.
- After: commit 9e84507beefdb56423dc48bc12e3ae46dea557f4. Clicking A shows A finished and 10s; clicking B shows B finished and 2.0s. Reload preserves the corrected durations.
- The local fixture supplies message payloads directly. It validates the actual component interaction and timeline transformations; backend persistence/live/history agreement is covered separately by the PR's database-backed tests.

Screenshots are unedited captures. Browser interaction was validated in a headed local browser; a separate local headless session captured the after images after the headed session's screenshot command timed out.
