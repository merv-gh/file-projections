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
`JOERN_HEAP` (4g). Workers are `ghcr.io/joernio/joern:*` containers that share the data volume
and the host Docker socket.

## API

| Method & path | purpose |
|---|---|
| `POST /jobs` | multipart: `metadata` JSON (`{name, branch?, export?}`) + `source` zip. Returns `{jobId, status}` (202). `export:false` keeps the raw `cpg.bin` and skips the neo4jcsv export (use this for CPG download / remote queries). |
| `GET /jobs` | list jobs |
| `GET /jobs/{id}` | job status (`queued`→`parsing`→`done`/`failed`, `progress`) |
| `GET /jobs/{id}/logs` | SSE stream of logs + progress |
| `GET /jobs/{id}/result` | tar.gz of the neo4jcsv export (default `export:true` jobs) |
| `GET /jobs/{id}/cpg` | the raw `cpg.bin` (only for `export:false` jobs, which keep it) |
| `POST /jobs/{id}/script` | multipart: `script` file + repeated `param` `k=v`. Runs `joern --script` against the job's CPG (the farm injects `cpgPath` + `out`) and returns the script's output. |
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
falling back to `joern-parse`.
