# factory loadgen: requests for load tests

`factory loadgen --db <url> [--per-api 40] [--skew uniform|zipf:s|hot:N] [--new-offset 0] [--out requests.json]`
reads a database filled with production-like data (`factory seed-volume`, or a copy of real data) and writes requests
for every API of the project (`--project DIR` for another project). It only reads the database.

## What is generated

| API | parameters |
|---|---|
| read (declarative) | the filters' values come from rows of the data the filters see (the pipeline before them: lookups, nests, logical tables, groups); equality / prefix hit existing rows, ranges span two rows (dates: a week). Fields without a filter: the spec's first OK request |
| read (escape-hatch SQL, compute) | the spec's first OK request |
| write: 新建 / 组合命令的新建 | **a new key per request**: the data generator's values for rows after the table's last row (new keys, references to existing parents, CHECKs satisfied) |
| write: 按键更新 / 按键删除 / 条件更新 | **an existing key**: rows read from the table — in the state the operation's condition wants, existing in other tables the same input is the key of, without child rows for deletes; the optimistic-lock version is the row's |
| 每项 (lists) | up to 3 elements; elements that share the request's key (e.g. one transfer's debits) come from one group of rows |
| message handler / state machine | the message's `data` (`kind: message`, `type` = the message type, `path: ""`): not HTTP — publish it to the service's queue |

Fields no column decides (amounts, free text) take the spec's first OK request's value, so the request passes the
API's validation.

`--skew` decides how the existing keys are spread over the requests (updates, deletes, reads):

| skew | keys |
|---|---|
| `uniform` (default) | every request a different key (distinct rows; reads: a random sampled row) |
| `zipf:s` (s > 1) | key k with probability ∝ 1/kˢ: a few hot keys and a long tail (`zipf:1.2` ≈ realistic popularity) |
| `hot:N` | every request one of the first N keys: an explicit hot-key scenario (row lock waits, version conflicts) |

New keys (inserts) are always distinct. Two files generated from the same database before either is sent share their
new keys: give them different `--new-offset` (e.g. 0 and 100000), or regenerate after each run.

## Output

```json
[{"api": "jobs.create-job", "path": "/factory.jobs.v1.JobService/CreateJob", "body": {"id": 20001, "job_type": "v222"},
  "kind": "write", "single_use": true}]
```

- `kind`: `read` / `write` / `message`
- `single_use`: sending it again does not do the same work (a new key that now exists, a deleted row, a version that
  has moved on). Send each such request once, in order — not a random pick with repetition.

## How k6 consumes it

Reads can be picked at random (they are repeatable); single-use writes must be consumed once each, so a load test
uses two scenarios:

```js
import http from "k6/http";
import exec from "k6/execution";
import { SharedArray } from "k6/data";

const all = JSON.parse(open(__ENV.REQUESTS || "./requests.json"));
const reads = new SharedArray("reads", () => all.filter((r) => r.kind === "read" || (r.kind === "write" && !r.single_use)));
const writes = new SharedArray("writes", () => all.filter((r) => r.kind === "write" && r.single_use));
const headers = { "Content-Type": "application/json" };

export const options = {
  scenarios: {
    reads: { executor: "constant-arrival-rate", exec: "read", rate: 50, timeUnit: "1s", duration: "2m", preAllocatedVUs: 50 },
    // shared-iterations: every write exactly once (iterations ≤ the number of writes)
    writes: { executor: "shared-iterations", exec: "write", vus: 8, iterations: writes.length, maxDuration: "2m" },
  },
};

export function read() {
  const r = reads[Math.floor(Math.random() * reads.length)];
  http.post(__ENV.BASE + r.path, JSON.stringify(r.body), { headers, tags: { api: r.api } });
}

export function write() {
  const r = writes[exec.scenario.iterationInTest];
  http.post(__ENV.BASE + r.path, JSON.stringify(r.body), { headers, tags: { api: r.api } });
}
```

For a sustained write rate, generate enough single-use requests (`--per-api` ≥ rate × duration ÷ the number of write
APIs). Requests with `kind: message` go to the message broker instead (a publisher, not k6's http). A script that
validates the mix by sending every request once (`validate.py` style) must skip single-use writes, or run against a
copy of the database.

Expected answers with a skew: `hot:N` makes deletes `not_found` after the first, version-locked updates `aborted`
(optimistic lock), inserts unaffected; that is the scenario, not a broken mix.
