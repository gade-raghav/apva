# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue. Report privately via GitHub's
[private vulnerability reporting](https://github.com/gade-raghav/apva/security/advisories/new).

We aim to acknowledge reports within 3 working days and to publish a fix or mitigation
within 90 days.

## Supported versions

Only the latest minor release receives security fixes during pre-1.0 development.

## Design notes

APVA v0.x is **read-only**: it queries Prometheus and serves recommendations. It does not
modify cluster workloads, and its Helm chart grants no write RBAC.
