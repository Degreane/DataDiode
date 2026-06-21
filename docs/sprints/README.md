# Sprints

Agile sprint records for DataDiode. Each sprint is ~2 weeks and gets one file:

- `sprint-NN-<slug>.md` — plan, backlog, daily notes, retro.

## Cadence

- **Sprint length:** 2 weeks.
- **Ceremonies (lightweight, async-friendly):**
  - **Planning** — Day 1: pick stories from the backlog into the sprint.
  - **Daily** — async standup in the sprint doc (what shipped / what's next / blockers).
  - **Review** — last day: demo / commit walk-through.
  - **Retro** — last day: what worked, what didn't, one change for next sprint.

## Definition of Done

A story is "done" when:
1. Code merged to `main`.
2. Tests pass on Linux, Windows, macOS (CI matrix).
3. Docs updated (README, ADR, or sprint doc as appropriate).
4. No new dependency added without an ADR.

## Index

- [Sprint 00 — Kickoff & Research](sprint-00-kickoff.md) — closed
- [Sprint 01 — MVP: tx → UDP → rx in two LXC containers](sprint-01-mvp.md) — closed (2026-06-21)
- [Sprint 02 — File operations & polish](sprint-02-fileops.md) — closed (2026-06-21)
- [Sprint 03 — Encryption on the wire](sprint-03-encryption.md) — closed (2026-06-21)
