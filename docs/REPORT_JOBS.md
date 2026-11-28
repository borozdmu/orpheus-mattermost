# Continuing an accepted Mattermost report

The operator mounts one read-only JSON card at `MM_REPORT_JOB_CARD`. The file
and its directory must be root-owned and protected against group or other
writes. With no card the ordinary mention and file workflows remain available.

The card records `schema: orpheus.mattermost.report-card.v1`, `job_id` (a UUID),
`workflow`, `channel_id`, `root_post_id`, `actor_id`, `method_sha256`,
`source_id`, `source_revision` and `gateway_url`. Its gateway is
`https://beta-kord.skillum.ai/mattermost-report/v1/jobs/`. The operator checks
the method's SHA-256 and confirms its intended source and native reader group.
The named author posts a reply with exactly `@<bot> /report <job_id>` and
attaches the checked program. A stage reads the program from this attachment.

Each stage runs the program in a separate, normally authorized Core session.
The hook supplies the native `.orpheus/mattermost/report-job.json` and, after
the first stage, the exact `report-checkpoint.zip` confirmed by the receiver.
The program writes one checkpoint with a strictly greater position, or the
final `MQL_2026_RetailCRM.xlsx`, in its own outbox. The receiver checks the
current stage, author, thread, audience, source, file digest and attachment
receipt before publication. A checkpoint is visible to members of the same
closed thread. The next stage starts automatically after the previous run
has ended. An edited or deleted original command cannot authorize more work.

A short-lived stage credential reaches only the receiver's method, checkpoint
and result handlers. It is absent from the calculation subprocess. Each
checkpoint restore, upload and publication obtains its own fresh decision
from Core. The public edge uses the existing HTTPS entry point. Its upstream
response timeout must exceed the 210-second receiver operation budget.

The receiver reconciles uncertain delivery against native posts, checks
attachments, and never sends a generic repair post for an accepted report.
Stages may retry a verified pre-upload temporary failure after 1, 2 and
4 minutes. The receiver checks the current grant each time. Without a
confirmed checkpoint, workbook or safe retry witness, it sends one short
technical stop notice subject to the original audience check.
