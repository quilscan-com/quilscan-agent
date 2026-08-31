# Qclient Manage and Token Status Design

## Goal

Adapt the Agent and browser Manage view to the latest local
`quilscan-qclient` output, fix idle-worker allocation column shifts, expose
materialization health, and publish claimable rewards and token balance in the
existing cumulative `node_status` snapshot.

## Source of Truth

The command contract comes from `/Users/otteralpha/qiao/quilscan-qclient` at
the current `main` head. Older qclient output formats are not supported.

The Agent runs these commands:

```text
qclient node prover manage --once
qclient token claimable-rewards --json --config <config-dir>
qclient token balance --config <config-dir>
```

## Manage Parsing

The latest allocation row schema is:

```text
Filter? Provers Ring Size[MB] Shards Mat Lag State Reward[Q/f] Worker Status Mode? NextAction? DefaultAction?
```

`Filter` is empty for an unallocated idle worker. The parser must not infer
filter presence from row length because the new `Mat`, `Lag`, and `State`
columns make an empty-filter row long enough to resemble a filtered legacy
row. It validates the latest fixed-value sequence at both candidate offsets
and accepts the candidate whose numeric, materialization-state, reward, worker,
and status fields satisfy the latest schema.

The Agent allocation payload adds:

- `materializedFrame`: raw `Mat` text
- `lag`: raw `Lag` text, including `-` when the node has no latest frame
- `materializationState`: `Unknown`, `Unmat`, `Lag`, or `Current`

All existing allocation fields remain available. For an idle worker with no
filter, `filter` stays empty, `worker` contains the core ID, and `status`
contains `idle`.

## Token Status Collection

Token collection runs in a dedicated background loop. It executes immediately
after Agent startup and then every one minute. It is separate from the existing
node verification, allocations, disk-usage, metrics, and version-poll loops.

Claimable rewards use the qclient JSON response and publish the QUIL-denominated
`balance_quil`. Token balance scans the `Total balance: <value> QUIL` line and
preserves the decimal text rather than converting it to a floating-point
number.

Successful results merge into the existing cumulative `node_status` snapshot:

```text
qclient_claimable_rewards
qclient_claimable_rewards_refreshed_at
qclient_token_balance
qclient_token_balance_refreshed_at
```

The timestamp for each value advances only when that command succeeds.

## Failure Isolation

The two Token commands are independent. A timeout, non-zero exit, malformed
output, missing config, missing qclient binary, or RPC failure:

- affects only that command's current refresh;
- does not overwrite its last successful value or timestamp;
- does not set `qclient_status` to unavailable;
- does not block or delay the normal Agent verification and `node_status`
  publishing loop;
- does not prevent the other Token command from succeeding and publishing.

The dedicated loop performs no overlapping Token refreshes: one cycle finishes
or times out before its next one-minute tick is handled.

## Browser Manage View

The browser Manage allocation table adds `Mat`, `Lag`, and `State` columns
between `Shards` and `Reward [Q/f]`. The normalization layer reads the new
Agent fields directly.

The Manage view also shows the two node-level values:

- `Claimable Rewards`
- `Token Balance`

Missing values render as an em dash. They are display-only status values and do
not participate in allocation actions.

## API Contract

The OpenAPI `QClientAllocation` schema documents the three materialization
fields. `AgentNodeStatus` documents both balance values and their independent
refresh timestamps. Decimal QUIL values remain strings to avoid precision
loss.

## Testing

Agent tests cover:

1. a latest-format filtered allocation row;
2. a latest-format idle-worker row with an empty filter;
3. claimable-rewards JSON parsing and exact command arguments;
4. token-balance text parsing despite qclient setup output preceding it;
5. one Token command failing while the other publishes successfully;
6. both Token commands failing without changing prior `node_status` values;
7. the one-minute default cadence without overlapping refreshes.

Frontend checks cover normalization and rendering of `Mat`, `Lag`, `State`,
`Claimable Rewards`, and `Token Balance`, plus updated table column spans.

## Scope

Production changes are limited to:

- `quilscan-agent` qclient parsing and independent token-status collection;
- `quilscan-agent` reconcile wiring and tests;
- `quil-explorer` Manage normalization/rendering;
- the corresponding OpenAPI and interface documentation.

No qclient source, node source, build/release workflow, installation behavior,
or allocation action behavior changes in this task.
