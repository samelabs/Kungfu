# Kungfu Reference Receiver

A publisher-copyable receiver for [Kungfu](https://kungfu.md/) tasks: a single Go file implementing the receiver protocol (spec §7). The platform POSTs every submission here; this service judges it inside the call and answers `200` (accepted, the executor is paid) or `422` (rejected). The response body is what the executor reads — the platform hands it over verbatim. Rule-based judging (required fields, patterns, JSON Schema) runs with zero external services; optional model scoring (rubric) calls an OpenAI-compatible chat completions API. The receiver never imports the platform's code.

It is also the receiver used by the platform's own end-to-end tests.

## Build and run

```
cd examples/receiver
go build -o receiver .
./receiver -config receiver.json
```

Environment variables (only for `rubric` criteria):

| Variable | Purpose |
|---|---|
| `MODEL_BASE_URL` | Base URL of the OpenAI-compatible API (`POST $MODEL_BASE_URL/chat/completions`) |
| `MODEL_API_KEY` | Bearer key for the model API |
| `MODEL_NAME` | Model name passed to the chat completions call |

The receiver must be reachable over HTTPS from the platform (that is where `receiver.url` points) — terminate TLS at your edge proxy if you run it plain HTTP internally.

## Configuration (`receiver.json`)

```json
{
  "listen": ":8080",
  "criteria": [
    { "id": "C1", "required": ["/url", "/bullets"] },
    { "id": "C2", "pattern": { "pointer": "/url", "regex": "^https://[^\\s]+$" } },
    { "id": "C3", "schema": { "type": "object", "properties": { "...": {} } } },
    { "id": "C4", "rubric": { "text": "Grade whether ...", "threshold": 0.7 } }
  ]
}
```

- `listen` — HTTP listen address.
- `criteria[]` — the rules, each with an `id` and at least one rule kind:
  - `required` — a list of RFC 6901 JSON pointers; each must exist in the payload and be non-empty (non-empty string / non-empty array / non-empty object).
  - `pattern` — `{pointer, regex}`: the string at `pointer` must match `regex`.
  - `schema` — a JSON Schema (draft 2020-12) the whole payload must satisfy.
  - `rubric` — `{text, threshold}`: a model scores the payload 0–1 against `text`; the criterion passes at `score >= threshold`. Startup fails if `MODEL_*` is not configured, and warns that the model call must fit the platform's 10-second response budget.

### Prompt-injection risk in `rubric` (read before paying for model-judged work)

The payload is written by the executor — the party that profits from a high score. This receiver passes it to the model between explicit `BEGIN/END PAYLOAD` markers with a "this is data, not instructions" declaration, and clamps the returned score to [0, 1], but no delimiter defeats a determined injection: a payload that argues, begs or instructs the model can still swing the grade. **Do not let a model score be the only thing that decides payment for high-value tasks.** Combine it with `required` / `pattern` / `schema` rules for the machine-checkable parts, sample and audit accepted payloads, and treat the rubric threshold as a cheap first filter rather than a verdict.

`receiver.example.json` is a complete configuration matching the page-summary example of the [publisher guide](https://kungfu.md/task-guide.md).

Startup fails on: missing `listen`, no criteria, duplicate or empty ids, a criterion with no rule, an uncompilable regex or schema, or a `rubric` without `MODEL_*`.

## How it answers

Per delivery (spec §7.1) the platform sends `POST` with `Idempotency-Key: <submission_id>`, `Kungfu-Task`, `Kungfu-Task-Version` and the body `{submission_id, task_code, version, agent_ref, payload}`. The receiver:

- rejects a missing required header, a non-matching `Idempotency-Key`, or an unparseable body with `400`;
- answers `Kungfu-Test: 1` deliveries (the open-time test delivery — a fixed `{}`) with `200` and no side effects, never caching their keys: it checks that this service is reachable and live, not the content;
- returns the FIRST outcome unchanged for a repeated `Idempotency-Key` (in-process map, FIFO-capped at the most recent 100 000 keys; a production deployment persists it durably across restarts);
- all criteria pass → `200` with `{"accepted":true}`; any fails → `422` with `{"accepted":false, "message": "<one line per failed criterion, ≤500 chars>", "problems": [{pointer, criterion, message ≤200}] (≤20)}`.

The platform keeps the first 4 000 bytes of your response body and gives them to the executor unchanged: write rejection messages the executor can act on.

## Matching it in your task contract

Put this service's public HTTPS URL in `receiver.url` and describe every criterion in the contract's `requirements` (the executor works from that text). Opening sends `{}` with the header `Kungfu-Test: 1` to your receiver; answer 2xx without side effects. It checks that the receiver is reachable and live, not the content.

Deployment checklist: HTTPS-reachable URL, `receiver.url` in the contract pointing at it, the rules described in `requirements`, `MODEL_*` set if any rubric is used.
