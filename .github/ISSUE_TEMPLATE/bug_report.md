---
name: Bug report
about: Something is behaving unexpectedly
labels: ["bug", "needs-triage"]
---

## What happened

<!-- A clear, factual description. Keep speculation out of this section. -->

## What you expected

<!-- One or two sentences. -->

## Reproducer

<!--
  The smallest sequence of steps that triggers the bug.
  - exact `curl` (or other client) invocation, with PII redacted
  - relevant install.yml / runtime.yml deltas from the defaults
  - state of the signal_messages row, if relevant: status, attempts, last_error
-->

```bash
# example
curl -F 'metadata={"externalId":"x","recipient":"+380...","content":"hi"};type=application/json' \
     http://localhost:8083/api/v1/signal-messages
```

## Logs

<!-- Paste relevant log lines (with timestamps). Trim aggressively. -->

```
```

## Environment

- go-signalium commit / version:
- Postgres version:
- MinIO version:
- signal-cli version:
- OS / arch:

## Workaround

<!-- If you found one, share it so other users hitting the same bug can unblock themselves. -->

## Anything else?

<!-- Screenshots, hypotheses, similar issues you found. -->
