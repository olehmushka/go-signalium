# Architectural decision records

Every architectural-level decision lives here as a numbered, immutable record. ADRs document *why* a path was chosen and what alternatives were considered, so future contributors can challenge the reasoning instead of guessing it.

## Index

| # | Title | Status |
|---|---|---|
| [0001](./0001-rest-as-inbound-trigger.md) | REST as the inbound trigger | Accepted |
| [0002](./0002-polling-over-webhooks.md) | Polling over webhooks for terminal status | Accepted |
| [0003](./0003-multipart-attachments.md) | Multipart upload for attachments, staged to local MinIO | Accepted |
| [0004](./0004-witchcraft-full-framework.md) | Adopt Palantir Witchcraft at the full-framework tier | Accepted |
| [0005](./0005-fx-wrapping-witchcraft.md) | fx owns `main`, witchcraft is a fx-managed component | Accepted (with documented contingency) |
| [0006](./0006-single-recipient-or-group-per-message.md) | One recipient OR one group per message | Accepted |
| [0007](./0007-atlas-for-migrations.md) | Atlas (ariga.io) for database migrations | Accepted |
| [0008](./0008-conjure-bypass-for-multipart.md) | Bypass Conjure for the multipart upload endpoint | Accepted |

## When to write a new ADR

Write one for any decision that:

- introduces or removes a runtime dependency (broker, datastore, framework, language tool);
- changes a public contract (REST shape, error model, status state machine);
- changes a cross-cutting convention (error handling, logging, testing strategy);
- locks in a non-obvious operational stance (auto-migrations, advisory locking, signal-handler ownership);
- would surprise a contributor reading the code without context.

Do **not** write one for a refactor, a bug fix, or a small library upgrade. Those belong in commit messages and PR descriptions.

## Process

1. Copy [`_template.md`](./_template.md) to `NNNN-short-kebab-slug.md` using the next free integer.
2. Open it as part of the PR that implements (or proposes) the decision.
3. Mark `Status: Proposed` until merged; flip to `Accepted` on merge.
4. Never edit a merged ADR's decision — supersede it with a new one and add a `Superseded by: NNNN` line at the top of the old.

## Numbering

ADRs are zero-padded to four digits. Numbers are sequential and never reused. If two PRs race for the same number, the second rebases.
