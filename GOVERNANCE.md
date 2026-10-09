# Governance

## What this repository holds

| Part | Authority | Released as |
|---|---|---|
| [`kungfu.md`](kungfu.md) | The protocol. Normative. | tags `protocol/vMAJOR.MINOR.PATCH` |
| [`kungfu.zh-CN.md`](kungfu.zh-CN.md) | Informative translation of the protocol. | with the protocol |
| Kungfu 3.0 (Go code, `web/`, `migrations/`) | The reference implementation. Bound by the protocol, never above it. | application tags `vMAJOR.MINOR.PATCH` |
| [`docs/`](docs/README.md) | Engineering records of the implementation. Not normative. | — |

When two sources disagree: `kungfu.md` wins over the implementation, the implementation's current specifications (such as `docs/task-spec-1.0.md`) win over historical records, and the English protocol text wins over any translation.

One repository keeps the protocol, its reference implementation and their full history together. History is never rewritten: commits, failures, fixes and tests are part of the evidence.

## Changing the protocol

1. **Propose in public** — an issue or a pull request that changes `kungfu.md`. State the problem, the proposed rule, its compatibility impact (major, minor or patch; see §11 of the protocol) and how it can be verified. A reproducible case outweighs an argument.
2. **Review** — a change is accepted when it is consistent with the rest of the specification, its effect on existing behavior is stated, and the translation is updated in the same pull request.
3. **Release** — a released protocol version is an immutable text under its tag. Drafts carry "Draft" in the document header.

An implementation that finds a contradiction or a gap in the protocol proposes a revision. It does not fill the gap with its own rule.

## Changing the reference implementation

Fork, branch, run `scripts/dev.sh test`, open a pull request. See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md). A change that alters protocol-visible behavior updates [`docs/conformance.md`](docs/conformance.md) in the same pull request.

## Conformance claims

Any implementation, including Kungfu 3.0, claims conformance through a statement as defined in §10.3 of the protocol: specification version, implementation version, profiles, unmet rules and evidence. Kungfu 3.0's statement is [`docs/conformance.md`](docs/conformance.md).

## Languages

English is the language of the normative text and of the repository's primary documents. Translations are informative and are kept in sync in the same pull request as the change they translate.

## Maintainers

The project is maintained by samelabs. Security reports follow [SECURITY.md](SECURITY.md).
