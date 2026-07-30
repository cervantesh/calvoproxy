# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue for security vulnerabilities.

Instead, report it privately via GitHub Security Advisories
("Report a vulnerability" under the repository's **Security** tab), or by
contacting the maintainer directly. Include:

- a description of the issue and its impact,
- steps to reproduce,
- affected version/commit.

You'll get an acknowledgement as soon as possible, and we'll work with you on a
fix and coordinated disclosure.

## Scope notes

- CalvoProxy forwards requests to upstream providers (e.g. OpenRouter). Never
  commit real API keys — the proxy reads `OPENROUTER_API_KEY` from its
  environment.
- Prompts sent through the proxy leave the machine (to the upstream provider).
  Don't send secrets in prompts.
