<!--
Thanks for opening a PR.

Before submitting, please confirm:
  - [ ] `make lint` is clean.
  - [ ] `make test` passes with -race.
  - [ ] Public-contract changes are reflected in conjure/*.conjure.yml.
  - [ ] Schema changes were authored via `make migrate-diff` and atlas.sum was rehashed.
  - [ ] Architectural changes carry an ADR in docs/decisions/.
  - [ ] Docs that describe changed behaviour are updated in this PR.
-->

## Summary

<!-- One paragraph: what changed and why. The diff shows what; explain why. -->

## Motivation

<!-- What user-visible problem or developer-pain does this address?
     Link the issue if one exists. -->

Fixes #

## Approach

<!-- Brief sketch of how the change is structured. Call out non-obvious trade-offs. -->

## Testing

<!--
  - Which tests cover the new behaviour?
  - Anything tested manually that isn't covered automatically?
  - Did `make integration-test` run locally if you touched repo/migrations/sqlc?
-->

## Documentation / ADR

<!-- Link any docs/*.md sections updated. If this introduces or changes an
     architectural call, link the ADR in docs/decisions/. -->

## Breaking change?

<!-- Check one. -->

- [ ] No
- [ ] Yes — explained in CHANGELOG.md under "Breaking"
