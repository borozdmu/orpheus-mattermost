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

For an older calculation that stopped before accessing its outbox, the receiver
recognizes the single complete pre-upload failure line from the old hook. It
rechecks the previous native checkpoint and opens one diagnostic session for
the same stage and position. Another diagnostic failure stops that stage. The
child process reports only a predefined class of failure through its exit
status. The hook records this as `report-calculation-v1` without copying the
program's output, error text or input values. A confirmed new checkpoint
resumes the normal sequence.

A replacement method that produced the earlier hook's complete
`report-calculation-v1 source_validation` failure before submission can
receive one diagnostic attempt from the same confirmed checkpoint. Both
author commands, the source, the audience and the file are checked again.
The reviewed client's package lookup and source listing have separate,
fixed diagnostic codes. A further failure on that checkpoint stops the stage.
On ordinary stages, a confirmed `data_service_unavailable` during either
operation uses the same signed pre-submission retry and its existing limit
of three attempts. Access denials and unknown source errors stop the stage.
A signed submission attempt always takes precedence over a retry marker.


## Approving a replacement program

A separate root-owned, read-only JSON file at `MM_REPORT_METHOD_APPROVAL`
pins one transition. Its fields are `schema`
(`orpheus.mattermost.method-approval.v1`), `job_id`, `card_sha256`,
`from_sha256`, `to_sha256` and `file_name`. The original job card remains
in place. The same author attaches exactly that file to a new reply in the
original thread with the following command.

```text
@<bot> /report-upgrade <job UUID> <old program SHA-256> <new program SHA-256>
```

The receiver waits for a confirmed checkpoint. If the latest stage failed
before it submitted anything, the replacement can use its previous confirmed
checkpoint. An indeterminate submission requires a native delivery receipt.
A final workbook closes the job. The first stage of the new program, including
a safe retry of that stage, receives `method_transition` with the old and new
SHA-256. Subsequent stages use the new program normally. All restored bytes
come from the verified bot receipt. The receiver checks the original command
and approval reply again before material is released or the result is posted.

## Recording external file delivery

Before enabling staged reports, mount the publication journal as a writable
file at `MM_REPORT_PUBLICATION_JOURNAL`. Mount its operator registration
read-only at `MM_REPORT_RECOVERY_APPROVAL`. Ordinary conversations need
neither file. The receiver never creates or replaces a missing journal.

The registration schema is `orpheus.mattermost.report-journal.v1`. Its
remaining fields are `job_id`, `card_sha256`, `journal_id` (a fresh UUID),
`device` and `inode`. The operator creates a regular file owned by root
and the receiver's group, mode 0660, with one hard link. Write the following
single line with its terminating newline, then set the Linux append-only
file flag before starting the receiver.

```text
orpheus.mattermost.publication.v1 <journal_id> <job_id> <card_sha256>
```

The signed stage identifies that registration. The receiver records the
opening of the stage and each attempt to deliver a result. It holds a file
lock while checking prior records and synchronizing each new record to disk.
Fresh source and audience checks precede the external upload and post. A
lost reply leaves the attempt recorded across restarts. Only an exact native
delivery receipt can confirm the post. Incomplete records, changed
registration and unconfirmed sends stop further publication. The journal
contains identifiers and digests; the checkpoint stays in the original
thread.

For one audited interruption predating the journal, a root-owned
`orpheus.mattermost.report-recovery.v1` registration may additionally
certify the terminal run, its assignment, both author commands, checkpoint,
receiver version, audit interval and revocations. The two completed restore
decisions and the first CRM operation must precede revocation, with no
subsequent permission to upload or post. The operator verifies the complete
durable audit before issuing this time-limited certificate. The receiver
rechecks the commands, audience and native checkpoint, then records a
single recovery claim before requesting a fresh assignment. If the process
stops after the claim, check Core for that assignment. The receiver does
not resubmit an unconfirmed claim.

If the recovered program returns its signed temporary failure before
inspecting the outbox, the receiver can use the remaining ordinary retry.
The journal must contain the exact previous opening and no result intent.
The receiver rechecks the two commands, readers and native checkpoint,
then records the transfer of publication to a fresh invocation under the
same file lock used for a result. A delayed request from the former run
can no longer publish. A missing opening, an uncertain submission or an
unknown hook outcome stops the job. Each stage has at most three retries.

### Change the method after the remaining retry

After the last retry of an audited recovery, a second method can take over
from the same native checkpoint. The first approval stays mounted. A separate
root-owned read-only file at `MM_REPORT_SECOND_METHOD_APPROVAL` uses schema
`orpheus.mattermost.method-approval.v2`. Its fields bind the original job and
card, the first approval digest, both program digests, the new filename, and
the checkpoint digest, position and stage. The original person attaches the
approved program to a new post in the original thread with this command:

```text
@<bot> /report-upgrade-from-checkpoint <job UUID> <old program SHA-256> <new program SHA-256> <position> <checkpoint SHA-256>
```

The receiver verifies all three native posts, the original recipients and
current source access. It checks the previous native checkpoint and requires
a complete signed stop before output. The protected journal must still
contain the historical claim, its opening, its single handoff and the final
retry's opening. An exclusive lock serializes the method change with any
earlier writer. The new invocation may publish one result for that stage.
If Core did not acknowledge the new invocation, check its actual state
before any further action. A confirmed checkpoint starts the next ordinary
stage. An uncertain upload or incomplete journal stops the transfer.
