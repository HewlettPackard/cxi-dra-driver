# AGENTS.md

Index of AI-context files for `cxi-dra-driver`. Keep this file in sync whenever an agent, instruction, prompt, or skill is added, renamed, or removed (see the maintenance matrix in [.github/copilot-instructions.md](.github/copilot-instructions.md)).

## Agents

| Agent | Description |
|-------|-------------|
| [coding](.github/agents/coding.agent.md) | Implements and modifies Go code across `cmd/` and `internal/` |
| [pr-preparation](.github/agents/pr-preparation.agent.md) | Reviews a diff, maps components, drafts the PR description, flags risk |
| [documentation](.github/agents/documentation.agent.md) | Maintains README, CONTEXT, ARCHITECTURE, AGENTS, copilot-instructions, and docs/ in sync with code |
| [code-review](.github/agents/code-review.agent.md) | Reviews a diff for correctness, style, and domain-pattern adherence; usable by both PR authors (self-review) and reviewers |

## Instructions

| File | Scope (`applyTo`) |
|------|--------------------|
| [build-test.instructions.md](.github/instructions/build-test.instructions.md) | `**/*.go` |
| [code-style.instructions.md](.github/instructions/code-style.instructions.md) | `**/*.go` |
| [domain-patterns.instructions.md](.github/instructions/domain-patterns.instructions.md) | `**` |

## Prompts

| File | Purpose |
|------|---------|
| [pr-description.prompt.md](.github/prompts/pr-description.prompt.md) | Draft a PR description from a pending diff |
| [debug-driver.prompt.md](.github/prompts/debug-driver.prompt.md) | Triage a stuck claim, stuck pod, or leaked CXI service |

## Skills

| File | Purpose |
|------|---------|
| [cxi-dra-glossary](.github/skills/cxi-dra-glossary/SKILL.md) | DRA/CXI/CDI/NRI domain term glossary |
