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
