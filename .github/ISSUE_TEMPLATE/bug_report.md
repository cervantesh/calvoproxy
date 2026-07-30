---
name: Bug report
about: Report something that isn't working
title: "[bug] "
labels: bug
---

**Describe the bug**
A clear and concise description of what the bug is.

**To reproduce**
Steps / request that triggers it (redact any keys):

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer dummy" \
  -H "Content-Type: application/json" -d '{"model":"coding","messages":[...]}'
```

**Expected behavior**
What you expected to happen.

**Environment**
- CalvoProxy version / commit:
- OS + arch:
- How it's run (binary / Docker):
- Relevant `/health` output:

**Logs**
Paste relevant proxy logs (redact secrets).
