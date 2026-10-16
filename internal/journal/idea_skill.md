---
name: document-idea
description: Keep a living idea page while the user explores, prototypes or spikes something NEW that has no defined ticket yet — "what if we…", a proof of concept, an experiment, a proposal, a direction being tried out. Writes and keeps current .idea-brain/{{dir}}/<slug>.md in the layout of the team's ideas repository, after checking which ideas exist there already. Do NOT use when the session is implementing a defined ticket, fixing a bug, refactoring, reviewing or doing routine maintenance — that work has the decision log (`fride note`), not an idea page.
---

# document-idea

The team keeps its ideas in a git repository, one markdown page per idea in
its `{{dir}}/` folder. This skill keeps such a page for the idea being explored
in this workspace, **while** it is being explored, so that what was tried,
learned and decided is on record when the person clicks *Publish idea* in
FrIDE and the page goes to that repository.

## When this applies — and when it does not

Trigger only when the person is **ideating**: they are trying something out,
proposing a direction, prototyping, spiking, asking "what if", or exploring
whether an approach could work. Signs: no ticket or an exploratory one, the
goal is to find out rather than to deliver, scope is being discovered as you go.

Do **not** trigger when the person is **implementing**: a ticket with a defined
scope (a ticket key such as `PROJ-42` in the task name is a strong hint), a bug
fix, a refactor, a review, a dependency bump, a deploy. That work records its
reasoning with `fride note` instead. Idea pages full of ticket work bury the
real ideas.

Unsure? Ask once, briefly: "Is this an idea you are exploring, or a ticket you
are implementing?" Then remember the answer for the session. Never ask twice.

## First: does this idea exist already?

Before writing a page, look at what the ideas repository has:

```bash
fride idea ls                        # every idea page on main: path, status, title, description
fride idea cat {{dir}}/<slug>.md     # read one
```

An idea that already exists is **extended**, never duplicated: write your page
under the **same path** as the existing one, starting from its current content
(`idea cat`), with the new angle worked in and the person added to
`raised_by`. Publishing then updates that page. Only a really new idea gets a
new slug.

## What to write

One page: `.idea-brain/{{dir}}/<slug>.md`, `<slug>` a short kebab-case name for
the idea (not the workspace's name unless it *is* the idea). Screenshots and
other files that belong to the idea go to `.idea-brain/references/assets/` and
are embedded from the page with a relative link. Everything under
`.idea-brain/` is copied into the repository as it is laid out here.

Frontmatter:

```yaml
---
type: idea
title: Dynamic dashboards
description: One sentence — what the idea is, for the index.
status: draft          # draft = raw · in_review = explored · approved = decided · deprecated = dropped
raised_by: [Ada Lovelace]
tags: [dashboard, ai]
created: 2026-09-08
updated: 2026-09-08
---
```

Body sections, in this order, skipping empty ones:

- **What** — the idea in a few sentences; then what was actually built or tried
  here: approach, the files and modules it lives in, what works and what does
  not. Link every branch touched to its page on the git host (the repo's
  `git remote get-url origin`), and note the commit you looked at
  (`git rev-parse --short HEAD` in that repo).
- **Why** — the problem it solves, the alternative it beats, the evidence.
- **Open questions** — what is undecided, unverified, or needs someone else.
- **Related** — plain relative markdown links to other ideas, requirements,
  specs, sources (never wiki links).

A structure, flow, sequence or timeline that is clearer drawn goes into the page
as a small ```` ```mermaid ```` block — one idea per diagram, short labels, at
most ~15 nodes. Diagrams are text; revise them like a paragraph.

Write for the reader in six months: what the idea is, what was tried, what came
out of it. Plain, specific, no marketing.

## Keep it current, not chatty

Update the page at the moments that matter — the idea takes shape, an approach
is chosen or dropped, something surprising is learned, a milestone works — not
on every edit. Revise in place: rewrite the sentence, update the list. Never
append "Update: …" paragraphs. Bump `status` and `updated` when you touch it.

## Publishing

The page is published by the person, not by you: FrIDE's *Idea* panel has a
**Publish idea** button (`fride idea publish` does the same) that commits the
page(s) to the ideas repository on an `idea/…` branch, with whatever push
options the team configured (on GitLab, typically a merge request that merges
itself). Tell the person the page exists and where, once, when you create it.
