# Admission checkpoint

`admission_checkpoint` imports a drained channel's consumed trigger IDs from a
previous transport. It is an operator-controlled workflow field, not a Mattermost
message, access grant, or proof that the previous handler has stopped.

```yaml
admission_checkpoint:
  schema: mattermost-admission-checkpoint.v1
  channel_id: cccccccccccccccccccccccccc
  bot_id: bbbbbbbbbbbbbbbbbbbbbbbbbb
  epoch: 11111111-1111-4111-8111-111111111111
  floor_ms: 100
  replay_from_ms: 200
  consumed_post_ids: [pppppppppppppppppppppppppp]
include_ids: [cccccccccccccccccccccccccc]
reconcile_from: "1970-01-01T00:00:00.200Z"
draining: true
link_expansion:
  enabled: false
```

Use actual server timestamps and verified IDs. The example remains draining.
Configure exactly one channel, no DM/group routes, and disabled link expansion.
`reconcile_from` must equal `replay_from_ms` at exact millisecond precision.
Consumed IDs must be sorted and unique, up to 10,000; oversized checkpoints fail
instead of silently dropping IDs. The UUID epoch must be canonical and nonzero.

The bootstrap floor is exclusive; replay time is inclusive. All versions of a
consumed ID stay consumed. An unconsumed ID at the replay millisecond is eligible
even when it sorts before the last consumed ID. Source permissions and normal
mention rules still apply. Old posts are excluded from automatic context and
attachment selection; the first run of an ownership period includes only its
new triggers. Linked expansion cannot bypass the boundary.

The checkpoint participates in the effective workflow revision, so a new epoch
uses a fresh session. It carries no sandbox, previous file index, policy or model
memory. A restart in the same epoch uses normal authoritative Orpheus acceptance
history. Draining alone does not change the effective revision. Uncertain input
from an older in-memory workflow blocks admission; rejected input from a previous
period is not transferred into the new period.

The sample in `internal/config/testdata/channel-checkpoint.json` is produced by
the prototype Python exporter from a drained synthetic SQLite database. Tests
cover configuration rejection, the shared fixture, same-millisecond IDs,
edited consumed posts, clean protected sessions and lost acknowledgement/restart.

This change implements the import side only. Before a real handover, the owner
operator must independently verify remote runs, delivery receipts and no pending
writes; stop the previous ingress and its automatic restart; persist the period
and checkpoint; and start exactly one new ingress. Native export, reverse import,
crash-safe ownership transitions and live rollback still need acceptance. Do not
activate a workflow merely because checkpoint validation succeeds.
