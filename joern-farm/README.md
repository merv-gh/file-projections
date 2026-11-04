# joern-farm

A small HTTP service that runs [Joern](https://joern.io) in worker containers, so thin
clients (like [file-projections](../README.md)) can offload CPG building and queries to a
beefier machine. Parse once, keep the CPG, run many query scripts against it.

## Run

```sh
docker compose up -d --build      # farm-api on :9090 + one joern worker
curl localhost:9090/health
```

Config via env on `farm-api`: `PORT` (9090), `DATA_DIR` (/data), `WORKER_COUNT` (1),
`JOERN_HEAP` (4g), `QUERY_TIMEOUT_SECONDS` (300, range 1–3600), and
`QUERY_MAX_OUTPUT_BYTES` (67108864, maximum 536870912). `JOERN_HEAP` accepts a positive
JVM heap such as `4096m` or `8g`; it is passed to each parse, export and query JVM.
Invalid resource configuration fails startup. Workers are `ghcr.io/joernio/joern:*` containers that share the data volume
and the host Docker socket.

## API

| Method & path | purpose |
|---|---|
| `POST /jobs` | multipart: `metadata` JSON (`{name, branch?, language?, export?}`) + `source` zip. Language defaults to `java`; `javascript` (aliases `js`, `ts`, `typescript`) explicitly selects JS/TS. Returns `{jobId, status}` (202). `export:false` keeps the raw `cpg.bin` and skips the neo4jcsv export (use this for CPG download / remote queries). |
| `GET /jobs` | list jobs |
| `GET /jobs/{id}` | job status (`queued`→`parsing`→`done`/`failed`, `progress`) |
| `GET /jobs/{id}/logs` | SSE stream of logs + progress |
| `GET /jobs/{id}/result` | tar.gz of the neo4jcsv export (default `export:true` jobs) |
| `GET /jobs/{id}/cpg` | the raw `cpg.bin` (only for `export:false` jobs, which keep it) |
| `POST /jobs/{id}/script` | multipart: `script` file + repeated `param` `k=v`. Runs a trusted Joern script against the job's CPG (the farm owns `cpgPath` + `out`) and streams its bounded output after successful execution. |
| `DELETE /jobs/{id}` | delete a job + its artifacts |
| `POST /test` | parse a tiny built-in Java project (pipeline smoke test) |
| `GET /probe` | report which Joern tools each worker container has |

### Example (what file-projections does)

```sh
# 1. parse (keep the cpg)
JOB=$(curl -s -F 'metadata={"name":"svc","export":false}' -F 'source=@src.zip' \
      localhost:9090/jobs | jq -r .jobId)
# 2. wait
until [ "$(curl -s localhost:9090/jobs/$JOB | jq -r .status)" = done ]; do sleep 2; done
# 3. run a query script against the kept cpg
curl -s -F 'script=@entry-to-exit.sc' --form-string 'param=root=/x' \
     --form-string 'param=entry=@PostMapping' --form-string 'param=exit=\.save\(' \
     localhost:9090/jobs/$JOB/script
# 4. (optional) download the cpg.bin
curl -s localhost:9090/jobs/$JOB/cpg -o svc.cpg.bin
```

Parsing uses `javasrc2cpg -J-Xmx… --delombok-mode no-delombok` (single JVM) when available,
falling back to `joern-parse`. JavaScript/TypeScript uses `jssrc2cpg.sh` directly and never
falls back to the Java frontend. An unavailable JS frontend or unsupported language is
an explicit failure. Job status and list responses retain the selected language.

Script requests are capped at 2 MiB, with a 1 MiB script and at most 64 unique parameters
of 4096 bytes each. Replacing `cpgPath`/`out` is rejected. One query runs at a time;
concurrent requests receive HTTP 429. Diagnostics retain their last 128 KiB. A failed
script returns HTTP 500 even if it created partial output; partial output is discarded.
Successful NDJSON is streamed from disk, avoiding a whole-file allocation in the API.
Output above `QUERY_MAX_OUTPUT_BYTES` returns HTTP 413. A per-process file-size limit
also bounds query-created files (shell units may round this limit upward).

Queries require `setsid --wait` in the worker. Deadline or client disconnect stops the
query process group, including its JVM, and removes query files. Deadline failures
return HTTP 504; inability to confirm the cleanup command is reported in the error.
These are limits for the legacy trusted-script API, not a Scala sandbox or a complete
parse/query memory admission controller. Parse workers can overlap the one query;
size worker container memory for their combined heaps and native overhead.

Run focused regression tests with `cd joern-farm && go test -race ./...`. They cover
language/heap dispatch, partial-output failures, request/result bounds, parameter
ownership, admission, deadlines, and process-group cleanup through a fake Docker CLI.
