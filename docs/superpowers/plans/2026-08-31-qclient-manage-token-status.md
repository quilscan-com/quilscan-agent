# Qclient Manage and Token Status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Parse the latest local qclient Manage output, publish materialization and Token status through the Agent, and render those fields in the browser without Token failures affecting normal Agent reporting.

**Architecture:** `quilscan-agent` adapts the current `/Users/otteralpha/qiao/quilscan-qclient` CLI contract. Manage parsing validates the new fixed schema; Token commands run in an independent one-minute loop and merge only successful values into cumulative `node_status`. `quil-explorer` consumes and renders the new fields without changing allocation actions.

**Tech Stack:** Go, Vue 3, Vite, OpenAPI YAML

---

## Worktrees and Test Policy

- Agent: `/Users/otteralpha/.config/superpowers/worktrees/quilscan-agent/qclient-manage-token-status`
- Frontend: `/Users/otteralpha/.config/superpowers/worktrees/quil-explorer/qclient-manage-token-status`
- qclient source: `/Users/otteralpha/qiao/quilscan-qclient`

The user requires test files not to be committed. Go regression tests remain
local under the Agent's existing `*_test.go` ignore rule. The frontend contract
check lives under `/private/tmp`. Only production source and API documentation
are committed.

## File Map

Agent repository:

- Modify `internal/qclient/manage.go`: latest allocation schema.
- Create `internal/qclient/token_status.go`: Token command runners/parsers.
- Modify `internal/reconcile/loop.go`: isolated one-minute Token status loop.
- Local tests: `internal/qclient/manage_test.go`, `internal/qclient/token_status_test.go`, `internal/reconcile/qclient_token_status_test.go`.

Explorer repository:

- Modify `frontend/src/light/LightNodeManage.vue`: render materialization and balances.
- Modify `frontend/src/light/LightMyNodes.vue`: pass Token status values.
- Modify `docs/openapi.yaml`, `docs/agent-allocations-refresh-interface.md`, and `docs/frontend-api-guide-detailed.md`.
- Local check: `/private/tmp/check-qclient-manage-token-status.mjs`.

### Task 1: Parse the latest Manage schema

**Files:**
- Modify: `internal/qclient/manage.go`
- Local test: `internal/qclient/manage_test.go`

- [ ] **Step 1: Add failing latest-format tests**

Add an allocation with a real filter and an idle worker with an empty filter:

```go
func TestParseLatestManageIdleWorkerWithEmptyFilter(t *testing.T) {
	raw := `Allocations (1):
Select Filter Provers Ring Size [MB] Shards Mat Lag State Reward [Q/f] Worker Status Mode Next Action Default Action
[ ] 0 0 0.0 0 0 - Unknown ~0.00000000 2 idle a - -
Available Shards (0):`
	rows, err := ParseManageAllocations(raw)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	row := rows[0]
	if row.Filter != "" || row.Worker != "2" || row.Status != "idle" {
		t.Fatalf("idle worker columns shifted: %#v", row)
	}
	if row.MaterializedFrame != "0" || row.Lag != "-" || row.MaterializationState != "Unknown" {
		t.Fatalf("materialization columns shifted: %#v", row)
	}
}
```

The filtered case expects filter, `Mat=41`, `Lag=2`, `State=Lag`, reward,
worker, status, mode, and both actions at their current positions.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test ./internal/qclient -run 'TestParseLatestManage' -v
```

Expected: compile failure because the new Allocation fields are absent.

- [ ] **Step 3: Add allocation JSON fields**

```go
MaterializedFrame    string `json:"materializedFrame"`
Lag                  string `json:"lag"`
MaterializationState string `json:"materializationState"`
```

- [ ] **Step 4: Implement validated latest-only parsing**

Try the empty-filter candidate first and the filtered candidate second. Each
candidate must validate ten fixed values: integer Provers/Ring/Shards/Mat,
`Lag` as `-` or unsigned integer, state as `Unknown|Unmat|Lag|Current`, reward
with `~`, worker as `-` or integer, and non-empty status.

```go
const latestManageValueCount = 10

func parseAllocationRow(line string) (Allocation, bool) {
	fields := strings.Fields(line)
	offset := allocationMarkerOffset(fields)
	for _, valueOffset := range []int{offset, offset + 1} {
		row, ok := parseLatestAllocationValues(fields, valueOffset)
		if !ok {
			continue
		}
		if valueOffset == offset+1 {
			row.Filter = fields[offset]
		}
		return row, true
	}
	return Allocation{}, false
}
```

The value mapping is Provers `+0`, Ring `+1`, Size `+2`, Shards `+3`, Mat
`+4`, Lag `+5`, State `+6`, Reward `+7`, Worker `+8`, Status `+9`. Mode and
actions begin at `valueOffset + latestManageValueCount`.

- [ ] **Step 5: Verify GREEN**

Run:

```bash
go test ./internal/qclient -run 'TestParseLatestManage' -v
go test ./...
```

Expected: latest filtered and idle tests pass; full Agent suite exits zero.

### Task 2: Execute and parse Token commands

**Files:**
- Create: `internal/qclient/token_status.go`
- Local test: `internal/qclient/token_status_test.go`

- [ ] **Step 1: Add failing command tests**

Temporary qclient scripts must prove these exact invocations:

```text
token claimable-rewards --json --config /var/lib/quilscan/node/.config
token balance --config /var/lib/quilscan/node/.config
```

The claimable test returns:

```json
{"found":true,"balance_subunits":"12345000000000","balance_quil":"12.345000000000","units_per_quil":1000000000000,"cited_frame":700001}
```

The balance test returns setup lines followed by:

```text
Total balance: 8.500000000000 QUIL (Account 0x1234)
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/qclient -run 'TestRunClaimableRewards|TestRunTokenBalance' -v
```

Expected: compile failure because both runners are missing.

- [ ] **Step 3: Implement claimable JSON parsing**

`RunClaimableRewards` uses `newCommand`, WorkDir, context timeout, and a private
DTO with `balance_quil`. It returns an error for missing or non-decimal values.

- [ ] **Step 4: Implement balance parsing**

`RunTokenBalance` scans all lines using:

```go
var totalBalancePattern = regexp.MustCompile(`^Total balance:\s+([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL(?:\s|$)`)
```

It preserves captured decimal text and errors when no valid line exists.

- [ ] **Step 5: Verify GREEN**

```bash
go test ./internal/qclient -run 'TestRunClaimableRewards|TestRunTokenBalance' -v
go test ./...
```

Expected: command arguments, JSON parsing, noisy balance parsing, and suite pass.

### Task 3: Add the isolated one-minute status loop

**Files:**
- Modify: `internal/reconcile/loop.go`
- Local test: `internal/reconcile/qclient_token_status_test.go`

- [ ] **Step 1: Add failure-isolation tests**

Inject one failing runner and one successful runner. The failed field retains
its old value/timestamp while the successful balance becomes
`8.500000000000`. When both fail, no status frame is sent and no cached value
changes. Also assert the default cadence is `time.Minute`.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/reconcile -run 'TestQClientTokenStatus' -v
```

Expected: compile failure because the Token loop dependencies are missing.

- [ ] **Step 3: Add dependencies and cadence**

```go
QClientTokenTick time.Duration
QClientClaimableRewardsRunner func(context.Context, qclient.RunRequest, time.Duration) (string, error)
QClientTokenBalanceRunner func(context.Context, qclient.RunRequest, time.Duration) (string, error)
```

`qclientTokenTick()` returns the override when positive, otherwise one minute.

- [ ] **Step 4: Implement the independent loop**

`Run` starts `go l.runQClientTokenStatus(ctx)`. The new goroutine refreshes
immediately and then sequentially on its own ticker. It never holds `verifyMu`
or invokes `runVerify`, so Token timeouts cannot delay normal status reporting.

- [ ] **Step 5: Merge only successful fields**

Each successful command independently calls `updateNodeStatus` with its value
and its own RFC 3339 UTC timestamp. Errors do nothing: they do not update the
cache, timestamp, `qclient_status`, or the other Token field.

- [ ] **Step 6: Verify GREEN and races**

```bash
go test ./internal/reconcile -run 'TestQClientTokenStatus' -v
go test -race ./internal/qclient ./internal/reconcile
go test ./...
```

Expected: isolation tests, race detector, and full suite pass.

### Task 4: Render materialization and balances

**Files:**
- Modify: `frontend/src/light/LightNodeManage.vue`
- Modify: `frontend/src/light/LightMyNodes.vue`
- Local test: `/private/tmp/check-qclient-manage-token-status.mjs`

- [ ] **Step 1: Add a failing frontend contract check**

The temporary Node script asserts both Vue files contain:

```text
materializedFrame
materializationState
Claimable Rewards
Token Balance
:claimable-rewards="manageClaimableRewards"
:token-balance="manageTokenBalance"
colspan="15"
```

Run it and expect failure before production edits.

- [ ] **Step 2: Pass node-status values into Manage**

```js
const manageClaimableRewards = computed(() => String(nodeStatus.value?.qclient_claimable_rewards || ''))
const manageTokenBalance = computed(() => String(nodeStatus.value?.qclient_token_balance || ''))
```

Pass both as kebab-case props to `LightNodeManage`.

- [ ] **Step 3: Normalize new allocation fields**

Add string props for both balances and normalize:

```js
materializedFrame: String(row?.materializedFrame ?? '-'),
lag: String(row?.lag ?? '-'),
materializationState: String(row?.materializationState || ''),
```

- [ ] **Step 4: Render new UI fields**

Show read-only `Claimable Rewards` and `Token Balance` near the Allocations
header; missing values use an em dash and present values append `QUIL`. Insert
Mat/Lag/State between Shards and Reward, update both empty-row spans from 12 to
15, and widen the table minimum width without changing actions.

- [ ] **Step 5: Verify GREEN and build**

```bash
node /private/tmp/check-qclient-manage-token-status.mjs
npm run build
```

Expected: contract check and Vite build pass; only existing chunk warning remains.

### Task 5: Update API documentation

**Files:**
- Modify: `docs/openapi.yaml`
- Modify: `docs/agent-allocations-refresh-interface.md`
- Modify: `docs/frontend-api-guide-detailed.md`

- [ ] **Step 1: Document allocation fields**

Add string properties `materializedFrame`, `lag`, and
`materializationState` (`Unknown|Unmat|Lag|Current`) to `QClientAllocation`.

- [ ] **Step 2: Document Token status fields**

Add `qclient_claimable_rewards`, `qclient_token_balance`, and both independent
RFC 3339 refresh timestamps to `AgentNodeStatus`. Decimal QUIL values are
strings; failures retain prior successful values and timestamps.

- [ ] **Step 3: Update examples and validate**

Add the new fields to the allocation and node-status examples, then run:

```bash
git diff --check
rg -n 'qclient_claimable_rewards|qclient_token_balance|materializedFrame|materializationState' docs
```

Expected: clean whitespace and documented fields in all three files.

### Task 6: Verify and commit production files

**Files:**
- Agent commit excludes ignored `*_test.go` files.
- Explorer commit excludes `/private/tmp` checks and build outputs.

- [ ] **Step 1: Verify Agent**

```bash
git status --short --untracked-files=all
git diff --check
go test -race ./internal/qclient ./internal/reconcile
go test ./...
```

Expected: production changes only in `manage.go`, `token_status.go`, and
`loop.go`; ignored local tests do not appear; tests pass.

- [ ] **Step 2: Commit Agent with UTC metadata**

```bash
git add internal/qclient/manage.go internal/qclient/token_status.go internal/reconcile/loop.go
env TZ=UTC git commit -m "feat: report qclient manage and token status"
```

- [ ] **Step 3: Verify Explorer**

```bash
git status --short --untracked-files=all
git diff --check
npm run build
```

Expected: two Vue and three documentation files only; build passes.

- [ ] **Step 4: Commit Explorer with UTC metadata**

```bash
git add frontend/src/light/LightNodeManage.vue frontend/src/light/LightMyNodes.vue docs/openapi.yaml docs/agent-allocations-refresh-interface.md docs/frontend-api-guide-detailed.md
env TZ=UTC git commit -m "feat: show qclient materialization and balances"
```

- [ ] **Step 5: Verify clean branches**

```bash
git status --short --branch
git show -1 --format=fuller --no-patch
```

Expected: clean feature branches with Mercer335 and UTC metadata.
