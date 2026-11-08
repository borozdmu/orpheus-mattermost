<p align="center">
  <a href="https://orpheus-agents.github.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/orpheus-logo.svg">
      <img src=".github/orpheus-logo-light.svg" alt="Orpheus" width="240">
    </picture>
  </a>
</p>

# Orpheus + Mattermost

A Go connector for Mattermost. It requires Orpheus `v0.6.0` or newer.
It routes thread messages,
prepares conversation context and attachments, delivers clarifications to running
agents, and publishes progress, answers, and output files.

The connector has no local database: Orpheus records accepted inputs and execution
history, Mattermost stores source posts and delivery receipts, and AgentBox keeps
workspace files. WebSocket and SSE events accelerate reconciliation; REST replay
restores work after disconnects and restarts.

The connector sends each rendered Mattermost post as a separate item in `messages`.
Its [front matter](docs/CONFIGURATION.md#message-context) includes the author's
Mattermost email and the post's channel ID and name, including linked threads.
Its durable input contract (accepted post IDs and versions, attachment manifest,
and reply settings) is stored in the last item's `metadata`. Orpheus returns this
object in message history and events but sends only each item's `text` to the agent.
External keys identify the thread
and anchor post; they do not replace the accepted input snapshot.

## Run

1. Copy `.env.dist` to `.env` and set the Orpheus URL and credentials.
2. Copy `workflows/assistant.md` into an ignored `workflows.local/` directory and
   set `WORKFLOWS_DIR=workflows.local`. Edit its YAML front matter and Markdown
   instructions. Under `mattermost`, set `base_url` and `token_env` (the name of
   the ENV variable holding the bot token). Set `reconcile_from` once to your deployment's UTC cutover time.
3. Use a Linux AgentBox template with `python3` (3.9 or later). File hooks are
   embedded in the connector and installed automatically; no template rebuild or
   pip dependencies are needed.
4. Configure the Orpheus profile and service catalog. Include the Mattermost
   token's ENV name in a service or `HARNESS_ENV_ALLOWLIST` on API and worker,
   and supply its value to the worker for file hooks. Select tool services in
   the workflow's `services`, for example `services: [gitlab]`. Optional
   `env_from` adds individual ENV names. The connector does not need tool values.
5. Run `make start`; use `make stop` to shut down.

Process settings come from ENV. Mattermost URL and token reference, workflow policy,
and agent instructions live in the same Markdown file. The connector discovers
the bot ID and username through `/users/me`; no source ID, expected bot ID, or
mention-alias setting is needed.

`AGENTBOX_API_KEY` is optional. When set, the connector prepares attachment
clarifications immediately through SDK. Without it, text clarifications remain
immediate and batches containing files wait for the next run's `before_run`.
`AGENTBOX_API_URL` optionally overrides the SDK endpoint. Incoming/outgoing files
through ordinary hooks work without these variables. There is no automatic SDK
recovery of output manifests, reupload, or pause sweep.

Optional `transcribe_voice: true` workflows transcribe audio attachments before
admission through a configured HTTPS speech endpoint. The transcript stays with
its author and original attachment; credentials remain in the connector.
Local-policy workflows use their own input processor so masking still precedes
model access. See [voice configuration](docs/CONFIGURATION.md#voice-input).

The application does not read `.env` itself; Docker
Compose supplies it. See [configuration](docs/CONFIGURATION.md) and the
[sandbox file protocol](docs/FILES.md).

Suggested workflow text:

> Do not edit input files in place. If you need to attach a result, read
> `.orpheus/mattermost/current-run.json`, put the completed file in its `outbox`
> using an atomic rename, and then send your final answer. The connector
> attaches outbox files to the last answer of the run.

Binary commands: `serve`, `validate`, `healthcheck`, and `export-checkpoint`. The embedded Python
script handles `prepare-input`, `export-output`, and SDK `import-input`.
File hooks upload files but never create Mattermost posts.

For recovery after sandbox deletion, see [session continuation](docs/CONTINUATION.md).

## Transferring a channel

A disabled workflow can import an [admission checkpoint](docs/ADMISSION_CHECKPOINT.md)
from a drained previous transport. It preserves consumed IDs and the inclusive
replay boundary, and starts a clean ownership period. This is not a switch
command or a live handover acceptance. The read-only `export-checkpoint` command
verifies native history and delivery receipts for a return to the previous transport.

## Development

Docker, Compose, and Make are required. Go 1.27 and pinned checking tools run in
containers.

```sh
make fix gofix check
make smoke
```

`check` includes offline Python file-protocol tests and Go/Python integration
tests. It also covers formatting, `go fix`, module consistency, `go vet`, golangci-lint,
hadolint, dead code, unit/integration tests, the race detector, govulncheck, Trivy,
and compilation. `smoke` validates configuration, readiness, and graceful SIGTERM
using the production image and local mock dependencies.

## CI and releases

Pull requests and pushes to `main` run `make check` and `make smoke`.
Pushing a version tag such as `v0.1.0` builds and publishes
`retailcrm/orpheus-mattermost:0.1.0` for `linux/amd64` and `linux/arm64`.
Tags run the publishing job without repeating branch checks. Tag a reviewed,
passing commit. Docker Hub authentication uses the `DOCKERHUB_USERNAME` repository
variable and `DOCKERHUB_TOKEN` secret.

## Manual acceptance tests

External services and their credentials are never used in CI. Manual tests use
build tag `live`; `make test-live` reads the ignored `.env.live` file.
Set `MM_TEST_URL`, `MM_TEST_BOT_ID`, `MM_TEST_CHANNEL_ID`, and
`MATTERMOST_BOT_TOKEN` for the Mattermost transport test. The channel must be named
`dev-test-group`; the test creates and deletes its own posts and uploads.
For the agent scenario, also provide `ORPHEUS_TEST_URL`, `ORPHEUS_API_KEY`, and
`AGENTBOX_API_KEY`, with an Orpheus profile named `live` and an AgentBox template
named `codex` that runs the image-capable agent. This test creates disposable sessions and uses
the same embedded hooks as production, without installing a connector binary.

With `AGENTBOX_API_KEY`, `TestLiveSandboxFileHooks` also checks file preparation,
SDK clarification import, output publication and memory pause/resume directly on
the stock `codex` template, without requiring an Orpheus worker or model call.

The agent test covers image recognition in the initial run, clarification,
resumed run and linked thread, plus output-file delivery and publication replay
with fresh connector objects. Memory resume requires a template rebuilt with
envd `0.6.16` or later. Orpheus `v0.2.0` includes the hook polling fix and the
batched message API.

See [acceptance testing](docs/TESTING.md) for worker crash injection, offline fault
coverage and the 1,000-thread recovery workload. A passing local suite does not
establish compatibility with a different template or agent profile.
Supported limits and file-error behavior are documented in [FILES.md](docs/FILES.md).

## Progress history and model cost

With `BOT_FEEDBACK_SINCE` set to a Unix timestamp, newly accepted runs publish a
working post and user-facing agent progress. These short messages stay in the
thread after success, failure or cancellation. The connector never deletes
progress after an answer: Mattermost displays live deletions as "message deleted"
placeholders. Delivery receipts prevent duplicate progress and final answers
after retries or restarts. Restarting does not replay terminal progress. Typing
is refreshed every four seconds independently of model events.

`BOT_COST_FILE` points to a read-only JSON file with dated `fx` USD/RUB rates and
per-million-token `models` prices (`input`, `cached`, `output`). The final message
contains a model-cost estimate from run usage. Cached input replaces normal input;
reasoning is included in output. Missing usage, prices or a recent exchange rate
produce an explicit unavailable message. This is a standard short-context model
estimate, excluding cache-write surcharges, tools, sandbox compute and taxes.
Exchange rates are selected at run creation in Moscow time. Persisted final
receipts freeze the footer for retries. Pre-cutover runs keep their original
rendering, so existing immutable receipts remain valid.

On hosts with exhausted Docker default address pools, set `SMOKE_SUBNET` to an
unused explicit subnet for the temporary smoke-test network. The test removes
only the network and containers it created.

### Local tools and approved output files

A workflow with `access_context.local_services: true` installs only clients supplied
by its selected services. Each `ORPHEUS_LOCAL_TOOL_<NAME>` variable contains a
base64 JSON object with `directory`, `source_b64`, and `sha256`. Directories are
single hidden workspace directories; every client receives a private
`current-run.json`. The hook has no Mattermost credential or source attachment.
Unselected, modified or escaping tool definitions fail closed.

`access_context.local_provider_context: true` also forwards the observed identity
and reader projection to selected credential providers. It requires local services
and a `scope_context.policy_space_id` ending in `.` followed by the configured peer
bot ID. The peer must still be observed in the exact channel audience; it cannot
author inputs. Its separate peer field is omitted from the provider envelope.
Transport credentials remain absent, and local input processing is unchanged.

`local_before_run_python` is optional trusted administrator Python, not message
content. It runs only after the standard local initialization succeeds. It requires
local services, has a 64 KiB source limit, forbids NUL, and must fit the 64 KiB
encoded hook limit. Initialization or bootstrap failure stops the run. It does not
replace the local export hook. Changes invalidate the workflow revision.

`access_context.local_files: true` additionally requires exactly one selected tool
with `after_run: true`. The after hook runs its service-pinned bytes, ignoring
workspace edits. The tool must seal its approved results in the host relay.
The connector obtains that manifest and bytes only through
`/run/orpheus-output/output.sock`, verifies identities, size and hash, rechecks
channel access before upload and publication, and uploads using its own token.
Only the relay's text derivative formats are accepted. Existing bot receipts
prevent duplicate uploads after reconciliation. Incomplete exports are not
published. Transport secrets and original inputs never enter this output path.

The accepted input freezes whether the run exports local files. Reconciliation
also recognizes the exact pinned local export hook in immutable session history
for inputs accepted before that flag existed. Enabling output files later does
not reinterpret old hookless runs or add failure notices to delivered answers.
Runs admitted with local files still require a complete export before publication.

## External agent memory client

A selected static service can provide `AGENT_MEMORY_URL`, `AGENT_MEMORY_KEY`,
`AGENT_MEMORY_CLIENT_B64`, and `AGENT_MEMORY_CLIENT_SHA256`. Before each run,
the file hook verifies the bundled client checksum and installs it as
`.agent-memory/memory.py`, together with a private current session/run context.
The service key remains in the selected service environment. Without these
variables the hook behaves as before. Partial configuration or a bad checksum
fails preparation. This does not enable or expose Mattermost history or files
across channels; the common factual store and agent policy live outside this
connector.

## Deployment notes for cost-enabled builds

Builds with `BOT_COST_FILE` support need read access to the cost file and traversal
access to its mounted directory. See [cost-file permissions](docs/COST_FILE_PERMISSIONS.md)
for installation, repair and acceptance checks. This guide applies to the
cost-enabled runtime; it does not enable that feature in other builds.
