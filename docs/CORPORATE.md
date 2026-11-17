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
