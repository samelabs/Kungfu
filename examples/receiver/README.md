# Kungfu Reference Receiver

A publisher-copyable acceptance receiver for [Kungfu](https://kungfu.md/) tasks — a single Go file implementing the receiver protocol (spec §7) and producing §6.1 verdicts. Rule-based judging (required fields, patterns, JSON Schema) runs with zero external services; optional model scoring (rubric) calls an OpenAI-compatible chat completions API. This receiver never imports the platform's code: the platform is reached only over HTTP.

It is also the receiver used by the platform's own end-to-end tests.

## Build and run

```
cd examples/receiver
go build -o receiver .
./receiver -config receiver.json
```

Environment variables:

| Variable | Required when | Purpose |
|---|---|---|
| `KUNGFU_BASE_URL` | mode `async` | Platform base URL for the verdict write-back (`POST $KUNGFU_BASE_URL/api/v1/task_verdict`) |
| `KUNGFU_AGENT_KEY` | mode `async` | The publisher's Agent key (Bearer) for the write-back |
| `MODEL_BASE_URL` | any `rubric` criterion | Base URL of the OpenAI-compatible API (`POST $MODEL_BASE_URL/chat/completions`) |
| `MODEL_API_KEY` | any `rubric` criterion | Bearer key for the model API |
| `MODEL_NAME` | any `rubric` criterion | Model name passed to the chat completions call |

The receiver must be reachable over HTTPS from the platform (that is where `receiver.url` points) — terminate TLS at your edge proxy if you run it plain HTTP internally.

## Configuration (`receiver.json`)

```json
{
  "mode": "sync",
  "listen": ":8080",
  "criteria": [
    { "id": "C1", "required": ["/url", "/bullets"] },
    { "id": "C2", "pattern": { "pointer": "/url", "regex": "^https://[^\\s]+$" } },
    { "id": "C3", "schema": { "type": "object", "properties": { "...": {} } } },
    { "id": "C4", "rubric": { "text": "Grade whether ...", "threshold": 0.7 } }
  ]
}
```

- `mode` — `sync` or `async` (see below).
- `listen` — HTTP listen address.
- `criteria[]` — the rules, each with an `id` and at least one rule kind:
  - `required` — a list of RFC 6901 JSON pointers; each must exist in the payload and be non-empty (non-empty string / non-empty array / non-empty object).
  - `pattern` — `{pointer, regex}`: the string at `pointer` must match `regex`.
  - `schema` — a JSON Schema (draft 2020-12) the whole payload must satisfy.
  - `rubric` — `{text, threshold}`: a model scores the payload 0–1 against `text`; the criterion passes at `score >= threshold`. Startup fails if `MODEL_*` is not configured; a `sync` receiver with a rubric gets a warning (model latency risks the platform's 10-second response budget).

`receiver.example.json` holds one complete `sync` and one complete `async` configuration — copy whichever half you need.

Startup fails on: unknown `mode`, missing `listen`, no criteria, duplicate or empty ids, a criterion with no rule, an uncompilable regex or schema, a `rubric` without `MODEL_*`, or `async` without `KUNGFU_BASE_URL`/`KUNGFU_AGENT_KEY`.

## How it answers

Per delivery (spec §7.1) the platform sends `POST` with `Idempotency-Key: <submission_id>`, `Kungfu-Task`, `Kungfu-Task-Version` and the body `{submission_id, task_code, version, agent_ref, payload}`. The receiver:

- rejects a missing required header, a non-matching `Idempotency-Key`, or an unparseable body with `400`;
- answers `Kungfu-Test: 1` deliveries (the open-time test delivery) with `200` (sync) or `202` (async) and never judges them;
- returns the FIRST outcome unchanged for a repeated `Idempotency-Key` (in-process map; a production deployment persists it durably across restarts);
- `sync`: judges inside the call — all criteria pass → `200`; any fails → `422` with the rejecting verdict body;
- `async`: answers `202` immediately, judges in the background, and posts `{submission_id, verdict}` to `$KUNGFU_BASE_URL/api/v1/task_verdict` with `Authorization: Bearer $KUNGFU_AGENT_KEY`; a failed post is retried after 5 s, 30 s and 120 s, then logged and abandoned (past the review window the platform accepts by timeout).

The verdict body is always the §6.1 shape: `{accepted, criteria (failed ids), reason (≤500 chars, one line per failed criterion), retryable: true, annotations: [{pointer, criterion, message ≤300}]}`.

## Choosing sync or async

- `sync` when every criterion is mechanical (required/pattern/schema): the platform settles or rejects on your 2xx/4xx reply, no review window, no write-back credentials needed.
- `async` when a criterion needs a model (rubric) or anything slower than a few seconds: the submission parks in `under_review` and your verdict arrives through `task_verdict` within the review window.

## Matching it in your task contract

The `criteria` ids here mirror the contract you publish (`task_create`): use the same `C1…Cn` ids in `acceptance.criteria[]`, describe each rule there, and put this service's public HTTPS URL in `receiver.url`. Set `acceptance.mode` to `sync`/`async` to match this receiver's `mode` (a contract with a `judgment` criterion must be `async`). The open-time test delivery replays your first accepted example payload — the `sync` example config above matches the page-summary example contract in the [publisher guide](https://kungfu.md/task-guide.md).

Deployment checklist: HTTPS-reachable URL, `receiver.url` in the contract pointing at it, ids mirrored in `acceptance.criteria[]`, `MODEL_*` set if any rubric is used, `KUNGFU_*` set in async mode.
