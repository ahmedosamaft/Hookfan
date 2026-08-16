# Spec-Driven Development in hookfan

hookfan is built **spec-anchored**: a written specification precedes the code, lives in the repository, and is updated when the design changes. It is not discarded after implementation, and it is not a source from which code is generated.

This document explains what that means here, what it deliberately does not mean, and how to work within it.

## Background

The vocabulary comes from Birgitta Böckeler's [survey of spec-driven development tools](https://martinfowler.com/articles/exploring-gen-ai/sdd-3-tools.html) on martinfowler.com, which distinguishes three levels:

| Level | Specs are… | hookfan |
|---|---|---|
| **Spec-first** | written up front, then discarded | no — the plan stayed useful across every phase |
| **Spec-anchored** | kept and evolved alongside the code | **yes** |
| **Spec-as-source** | primary; generated code is never hand-edited | no |

We rejected spec-as-source on the article's own reasoning: it revives the failure modes of Model-Driven Development — inflexibility and non-determinism — and this codebase is written and reviewed by hand.

That article is a critical survey, not an endorsement, and its warnings shaped the process below more than its descriptions did. Three in particular:

- **Review fatigue.** "I'd rather review code than all these markdown files." A tool that emits eight markdown files per feature has moved the bottleneck rather than removed it.
- **Workflow sizing.** No single workflow fits every change. Applying a requirements-design-tasks pipeline to a one-line fix is "using a sledgehammer to crack a nut."
- **False control.** Elaborate templates create an *illusion* of control. Agents ignore or over-interpret instructions regardless of how much structure surrounds them; a longer spec is not a more obeyed one.

The rules below exist to get the benefit while avoiding those three traps.

## What counts as a spec here

Exactly three kinds of document, and no more:

**1. The build plan** — `docs/plan.md`

One document covering architecture, schema, phase order, and verification. It is the reference that phases 1–4 were built against. When a design decision changes, this file changes with it.

**2. Architecture Decision Records** — `docs/decisions/NNNN-title.md`

One short file per decision that was genuinely contested, recording the alternative and why it lost. These are append-only history: a superseded ADR is marked superseded, never rewritten. If a decision was obvious, it does not get an ADR.

**3. Executable specs** — the test suite

Behaviour that must hold is written as a test, not as prose. `TestConcurrentClaimsNeverDoubleDeliver` is the specification of the claim query; a paragraph asserting the same thing would be a claim, while the test is a check.

Everything else — user stories, task breakdowns, per-feature requirement documents, checklists — is deliberately absent. Each would be a file to review and to let drift.

## Sizing the process to the change

This is the article's workflow-sizing criticism, answered directly. Match the ceremony to the blast radius:

| Change | Process |
|---|---|
| Typo, comment, log message | Just the change |
| Bug fix, small refactor | A failing test first, then the fix |
| New behaviour in an existing component | Test + update the affected part of `plan.md` if the design shifted |
| New component, schema change, or protocol change | Written plan and agreement **before** code, then an ADR if a real alternative was rejected |

Nothing above the line requires a document. A one-line fix that arrives with a requirements document has cost more than it delivered.

## The loop for a substantial change

Used for each of the four phases so far.

1. **Write the plan.** Scope, approach, the schema or interface involved, and how it will be verified. Name the alternatives considered and why they lost.
2. **Get agreement before writing code.** This is the step that pays for itself. Reversing a design decision after implementation costs far more than an argument beforehand.
3. **Implement, with tests as the specification of behaviour.**
4. **Verify against the plan's own criteria** — the plan states how the phase will be demonstrated, and that demonstration is run, not asserted.
5. **Update the plan when reality differs.** This is what makes it spec-*anchored*. A plan that no longer matches the code is worse than no plan, because it is trusted and wrong.

Step 5 is not bookkeeping. Four decisions changed mid-build — the toolchain moved to Go 1.25, migrations moved to goose, primary keys became `bigserial`, and event splitting was removed in favour of `routing_keys` — and each was written back into the plan and the ADRs before the next phase started.

## When the spec and the code disagree

**The code wins, and the spec is corrected.** A spec is a design record, not a contract the code has violated.

Two real examples from this build:

- The plan specified the delivery claim as `WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)`. That form makes the outer `UPDATE` re-lock the rows and *block* on a concurrent worker instead of skipping. The code uses a CTE, and the plan was corrected — see [ADR 0005](decisions/0005-cte-claim-query.md).
- The plan called for splitting a multi-entry Meta batch into one event per entry. That would have made the forwarded body differ from the received bytes, invalidating the provider's own signature downstream. The design changed to one event per request with a `routing_keys` array — see [ADR 0003](decisions/0003-no-event-splitting.md).

Neither was a code defect. Both were specification defects found by implementation, which is one of the main things implementation is for.

## Working with AI assistants

Most of this repository was written with an AI assistant, which is the context the source article is examining. What actually helped:

- **Agree on the plan before generating code.** The expensive failure is not bad code, it is well-written code built on a design nobody checked.
- **Ask for the decision, not just the artifact.** "Which of these two and why" surfaces reasoning that can be reviewed; "write this" produces something that must be reverse-engineered.
- **Demand demonstration over assertion.** "It works" is worthless; a captured `curl` transcript, a query result, or a passing test is evidence. Every phase in this repository ends with output that was actually produced.
- **Treat agent output as a proposal.** During the build, one review agent claimed `sha256(raw_body)` dedupe would drop legitimate distinct events. It was wrong for this payload shape — every WhatsApp message carries a unique `id`, so genuinely distinct events differ in bytes — and the recommendation was declined, with a conflict counter added instead so the assumption is observable rather than assumed. Confident and wrong is a normal agent failure mode.

And what the article warns about, restated as a rule: **more specification is not more control.** If a document is not being read, it is not providing control — only the feeling of it. Delete it.

## Repository map

```
docs/plan.md               the build plan: architecture, schema, phases
docs/decisions/            ADRs, one per contested decision
docs/spec-driven-development.md   this file
CONTRIBUTING.md            how to build, test, and submit changes
internal/**/*_test.go      executable specifications of behaviour
```

## Honest limitations

Stated plainly, because a process document that only lists benefits is marketing.

- **This is a small codebase with one maintainer.** Spec-anchored development is cheap here. On a larger team the cost of keeping documents current rises, and some of this would not survive contact with that.
- **`plan.md` will drift.** It is maintained by discipline, not by a compiler. Nothing fails when it goes stale. The mitigation is that it is one file, not thirty — small enough that drift is visible.
- **No tooling enforces any of this.** No Kiro, no spec-kit, no Tessl. That is a deliberate choice: this project is smaller than the overhead those tools impose, and the article's review-fatigue criticism applies most sharply at this scale.
- **The ADRs are written by the people who made the decisions**, so they record the reasoning that was persuasive at the time, including the reasoning that later turned out to be wrong. That is the point of keeping the superseded ones.
