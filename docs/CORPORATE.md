# Corporate agent binding

A `role_ref` binding loads one Box `corporate-agent.v1` role. Model, sandbox,
instructions and runtime limits cannot be overridden by the transport.
`chat_access` records routes, stable account mappings and named modes; each mode
retains its selected services, input processor and history checkpoint.

Corporate admission freezes `corporate-assignment.v1`. The connector confirms
employee cards and signs the entire assignment with the server-only
`ORPHEUS_AUDIENCE_SECRET`. `POST /internal/audience/confirm` rechecks the current
configuration, audience and cards. It never issues a Kord token. New inputs
wait for the next protected run and obtain the requesting author's own grant.

The audience callback allows up to 50 seconds to read, validate and confirm
its request and three more seconds to return the witness. The core client
allows 60 seconds. The listener retains its usual deadline on other paths.
Failures and confirmations taking at least five seconds log a fixed result,
elapsed milliseconds and, after signature verification, the invocation ID.

Private channels and direct messages use their complete current membership.
Public channels require explicit `public_team_audience: true` and all current
users of their team, including guests and restricted accounts. Before scanning,
the connector proves effective global `view_members` using the observer's
current global roles and their current permission definitions. Both member
pages and stats can otherwise expose the same hidden subset. Two complete
scans detect changed membership, roles, scheme identity and observer permissions.
An unavailable permission check fails closed. No permission is granted by the
connector. Shared channels without proven complete remote-team coverage deny
protected admission; local members never stand in for remote readers.

The core chooses trusted callback endpoints, manages issue/renew/revoke and
installs the short-lived token. Provider alone calculates service rights.
After publication, later additions to chat/team history are the administrator's
responsibility; the connector does not delete or reclassify prior messages.

## Registered bot recipients

Corporate bindings can declare `access_context.bot_recipients` for bots that
read the channel but never author inputs or trigger model runs. This registry
does not replace any part of the current audience. An unregistered bot still
blocks admission. A registry entry may be absent from the current roster; only
observed recipients enter the assignment.

Each entry binds three independently confirmed identifiers: the immutable
Mattermost `id`, the registered `bot_id`, and its `owner_user_id`. Operators must
verify that exact source account against the trusted bot registry before arming
the route. Configuration alone does not establish a registration. Do not infer
ownership from names, roles, business email or membership. The trusted registry
must keep source account identity immutable for a registration ID; another
source account requires another registration.

Set `identity_projection.bot_owners_field` to the ownership field accepted by
the integration. The common Provider uses `reader_bot_owners`. The connector
projects every observed bot as `{bot_id, owner_user_id}` and includes its owner
in the reader identity array. Several bots may share one owner: the data scope
contains that owner once, while the signed assignment retains each bot as a
separate reader principal. Core and Provider recheck bot activity, ownership
and the owner's agent/data permissions before issuing or renewing credentials
and before protected operations. Revocation or a changed owner denies access.

This registry requires a matching corporate `agent_id` and
`verify_account_cards: true`. It cannot be combined with the fixed legacy
`peer_recipient` or local-policy modes. Human account confirmation remains
separate; bots and their owners are never submitted as invented human
Mattermost accounts. This configuration does not register bots or grant rights.

### Required trusted recipient check

Do not activate this registry until the trusted Provider supports the read-only
RPC `external.confirm_bot_audience` and the receiver's service credential permits
that method. The human `external.confirm_audience` method remains unchanged and
does not prove bot authorization. Existing Provider versions without the new
capability deny workflows that observe registered bot recipients.

The request uses the ordinary `request_id` envelope and only these parameters:

```json
{
  "provider": "mattermost",
  "agent_key": "example",
  "bots": {
    "pppppppppppppppppppppppppp": {"bot_id": 71, "owner_user_id": 77}
  }
}
```

The Provider must confirm the exact immutable source-account/registration tuple,
the active bot and owner, the owner's required agent entitlement and a stable
ownership revision. It returns exactly `request_id`, `ok: true` and a boolean
`result`. This method reads authority only: it must not issue or renew credentials,
register identities, edit grants or rely on a model's assertion.

The connector calls it for currently observed registered recipients at admission,
corporate reconfirmation and immediately before each output publication. A bot
revocation, changed owner or withdrawn owner entitlement therefore blocks output
even when the YAML and Mattermost roster remain unchanged. An unavailable method,
timeout, non-200 response or ambiguous result fails closed; configuration and human
card checks never substitute for the decision. Bindings without this registry do
not acquire a new Provider dependency.

## Existing state partitions

For an existing channel, set `chat_access[channel].state_ref` to its exact old
workflow ID. This is trusted operator configuration. One role and `agent_id`
remain shared. `ForChat` resolves current access and the channel mode before
using the old ID for native session namespaces, operation/invocation IDs and
local policy calls. Discovery, SSE hints and checkpoint export resolve the same
current channel; a denied alias grants no access. Existing notes are not copied
into a broader namespace. Provider still authorizes service memory. A changed
role or grant may require a fresh native session.

Once a binding uses `state_ref`, every route must declare one, including denied
routes. References must be unique and may not claim another root binding. A root
may retain its own ID for exactly one channel. Reload cannot change this mapping;
state handover requires drained work, checked journals and a restart. Agent run
capacity remains shared across its state partitions. Memory mode values retain
explicit false/empty settings; they do not create a new memory service.
Closing a route does not release its running or uncertain admission from the
agent's capacity limit. Its terminal state must be confirmed first.

Для работы с единым Provider задайте `KORD_PROVIDER_ENDPOINT_MODE=shared-file`. Подключите общий каталог `/etc/kord/provider-address` к `/run/kord-provider-address` только для чтения. Удалите индивидуальные адреса `ORPHEUS_ACCOUNT_DIRECTORY_URL` и `KORD_PROVIDER_URL`. Коннектор перечитывает файл `url` перед каждым подтверждением читателей и использует прежний служебный ключ. При проверке `POST /internal/audience/confirm` с заголовком `X-Kord-Route-Probe` журнал связывает номер запроса с явным ответом Provider. Ключи, тексты заданий и списки читателей в эти события не включаются.

## Подтверждение отдельного расписания

Если оператор подключил закрытый каталог `/run/approved-schedule-occurrences` только для чтения, приёмник выдаёт подписанное задание `corporate-assignment.v1` для одного запуска отдельного расписания Space. Протокол тот же, что в orpheus-telegram: `POST /internal/schedules/{schedule}/occurrences/{occurrence}` без тела, ключ вызывающей стороны лежит в `caller.key` того же каталога. Независимый доверенный регистратор (orpheus-box, `contour/schedule_channel_confirmation.py`) кладёт туда файл `<schedule>.json` с полями `Schedule`, `Occurrence`, `Slot`, `Source`, `Agent`, `Actor`, `Principal`, `ReadersDigest`, `Revision`, `Text`, `Chat` (номер канала Mattermost). Каталог и файлы принадлежат root и закрыты на запись.

Приёмник ничего не публикует в Mattermost. Он читает текущий канал и его участников, сверяет набор читателей с `ReadersDigest`, выдаёт задание с маршрутом `space-schedule:<schedule>` / `space-occurrence:<occurrence>` и сразу подтверждает его сам. Подтверждение Core для такого задания действует, пока файл регистрации тот же и не прошло пять минут от начала периода. Без каталога возможность выключена.

Для такого задания номер связи в событиях `kord.route.v1` вычисляется из номера запуска: `uuid5(occurrence_id, "audience/confirm")` без дефисов. Наблюдатель на сервере так связывает выдачу задания, подтверждение Core и решение Provider с тем же запуском Space. Явный заголовок `X-Kord-Route-Probe` имеет приоритет; обычные сообщения номер не получают.

### Joining and leaving a protected channel

The receiver reads current channel membership for each protected operation.
For a human missing from the initial mapping, it asks the account directory
to resolve every observed human and verify their existing agent access.
The directory credential for `<agent>-mattermost-audience` must allow both
`external.confirm_audience` and `external.resolve_audience_accounts`. Assigned
audience clients continue to use their registered agent and assignment policy.
A missing or ambiguous identity stops the operation. The next observation
excludes former members. Set `ORPHEUS_LIVE_AUDIENCE=false` to restore fixed
identity mappings during rollback.
