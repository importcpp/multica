# Agent activity outcome verification

Baseline: multica-ai/multica main at cf52ba33ccf97f756de9e6b9d364fc0a57dc5260.

These are Chromium screenshots from a local Vite fixture mounting the real shared AgentPerformanceSummary, ActivityTab and Sparkline components with production styles and locale resources. The fixture seeds the real React Query activity caches, using the real activity derivation and schema, and omits the unrelated Now/Recent sections. It does not run a coding agent or exercise shared-backend authentication.

The before image mounts baseline component and English locale sources with the same counts; the after images mount the changed components.

- before-mixed.png: 1 completed + 1 failed + 8 cancelled incorrectly displays 90%.
- after-mixed.png: same data displays 50%, 10 runs and 8 cancelled; cancellations are neutral in the bars.
- after-cancelled.png: 8 cancelled displays no success percentage and no successful bar segment.
- after-legacy.png: missing outcome fields display no success percentage or successful bar segment.
- after-zh-narrow.png: Chinese at 390x844, no horizontal overflow.

Keyboard focus on the success rate reveals the formula explanation.
Backend cancellation and aggregation are independently covered by TestAgentActivityOutcomes against local PostgreSQL: seven queued cancellations, one running cancellation, then completed and failed outcomes, plus old and active runs outside the aggregation. The exact regression test fails against baseline handler/query code and passes against the fix.
