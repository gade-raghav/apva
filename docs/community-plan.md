# Path to open source and the CNCF Sandbox

APVA is developed privately first, then opened to invited beta users, then released as open
source and proposed to the CNCF Sandbox.

## Stage 1 — Private build (now)
- Private repo `gade-raghav/apva`, Apache-2.0, CNCF Code of Conduct, DCO.
- Goal: v0.1 working end to end on kind (CI) and on at least one real cluster.

## Stage 2 — Private beta
- Invite 5–10 practitioners as read-only collaborators (Settings → Collaborators).
- Where to find them: CNCF Slack (#kubernetes-users, #sig-autoscaling, #tag-* channels),
  Kubernetes SIG Autoscaling meetings, local CNCF community groups / Kubernetes meetups.
- Ask each for: cluster size, GPU usage, whether recommendations matched their intuition,
  and permission to list them as an adopter later.
- Track feedback as GitHub issues labelled `beta-feedback`.

## Stage 3 — Public open-source release
- Flip repo to public; tag v0.1.0; publish the container image and Helm chart.
- Enable OpenSSF Scorecard and an OpenSSF Best Practices badge.
- Add ADOPTERS.md with beta users who agree to be listed.
- Blog post + demo video; submit a CNCF lightning talk / KubeCon + CloudNativeCon CFP.
- Present to the relevant CNCF TAG and at SIG Autoscaling.

## Stage 4 — CNCF Sandbox application
- Open an application issue in https://github.com/cncf/sandbox using its template.
- Be ready to show: a clear scope vs. existing projects (docs/positioning.md), public repo,
  governance, code of conduct, license, adopters, and more than one contributor.
- The TOC reviews applications in periodic batches; expect questions and possibly a
  request to present.
- Accepting means the project name/trademark is held by the Linux Foundation; the original
  authorship stays in git history, NOTICE and MAINTAINERS.
