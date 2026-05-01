# `.claude/` — Claude Code project setup

This directory contains the Claude Code configuration for the Beskar7 repository. Everything here is project-scoped and committed to the repo so that anyone (or any future session) opening Claude Code in this checkout starts from the same baseline.

## Layout

```
.claude/
├── README.md                       (this file)
├── agents/                         project-scoped subagents
│   ├── staff-architect.md          design calls, trade-offs, CAPI conformance
│   ├── golang-engineer.md          default executor for Go code changes
│   ├── security-engineer.md        TLS, RBAC, secrets, webhooks, supply chain
│   ├── qa-engineer.md              test strategy, coverage, race conditions
│   └── tech-writer.md              README, docs/, examples/, CHANGELOG
└── context/
    └── PROJECT_CONTEXT.md          living state of the project — punch list,
                                    decisions log, in-flight work
```

The repository-root `CLAUDE.md` is the entrypoint Claude reads automatically every session. It points at this directory.

## What goes where

| If you want to… | Edit |
|---|---|
| Change project-wide working agreements (build commands, code conventions, CAPI rules) | `CLAUDE.md` (root) |
| Track current bugs, decisions, in-flight work | `.claude/context/PROJECT_CONTEXT.md` |
| Tweak how an agent behaves | `.claude/agents/<agent>.md` |
| Add a new specialized agent | new file in `.claude/agents/` with YAML frontmatter (`name`, `description`, optional `model`, optional `tools`) |

## Using the agents

In a Claude Code session, the orchestrator dispatches the right agent based on the task. You can also explicitly request one:

> "Use the security-engineer to review this change."

Each agent reads `CLAUDE.md` and `.claude/context/PROJECT_CONTEXT.md` before doing substantive work. They're meant to compose: `staff-architect` decides, `golang-engineer` implements, `qa-engineer` plans tests, `security-engineer` audits, `tech-writer` documents.

## Maintaining the context file

`PROJECT_CONTEXT.md` is the project's working memory across sessions. It is **expected to drift if not maintained**, which makes it useless. The agreement is:

- Anyone closing a punch-list item moves it to "Recently closed" with the PR/commit ref.
- Anyone discovering a new issue adds it to the punch list with severity.
- Architectural decisions get a `D-NNN` entry in the Decisions log (append-only).
- The "Last meaningful update" timestamp at the top gets bumped on every non-trivial edit.

Drive-by edits to add a TODO are welcome. The file is owned by everyone.

## What's intentionally not here

- No `settings.json` / `settings.local.json` — those are personal-environment concerns and stay out of source control.
- No hooks. If we adopt hooks later (e.g., a pre-commit `make manifests` check), they'll go in `.claude/settings.json` with documentation here.
- No skills. Add only when there's a recurring workflow worth packaging.
