# AGENTS.md — lokutor-orchestrator

Instructions for everyone who changes this repository: people and coding agents alike
(`CLAUDE.md` points here).

## What this repo is

The Go library for full-duplex voice agents (`github.com/lokutor-ai/lokutor-orchestrator`): the
managed stream state machine, VAD and turn-taking (`pkg/turno`), barge-in and echo handling,
speculative STT/LLM, tool calls, the platform system prompt, and the STT/LLM/TTS providers.

It ships to production through `lokutor_tts`, which pins it as a **git submodule**
(`lokutor_tts/lokutor-orchestrator`, `replace => ./lokutor-orchestrator` in its `go.mod`) and
deploys with `scripts/prod/deploy.sh`. Nothing here deploys by itself — but every merged change
reaches real calls the next time `lokutor_tts` bumps the submodule.

Behaviour is evaluated outside this repo, in `orchestrator-testbench` (echo bench, prompt A/B on
production's request shape, replays of real calls).

## Gates: stop and get a person

An agent must not do any of the following. Describe what you would do in the issue, set
`status:needs-human`, and wait.

- **Bump the submodule in `lokutor_tts`** (that is the release: it changes live calls).
- **Tags and releases.** Versioning is not coherent today (a `v2.0.0` from February, `v1.26.0`
  newest, 131 untagged commits; production pins by SHA). Do not create tags until a person fixes
  the scheme.
- **Push or merge to `main`.**
- **Prompt changes** (`renderSystemPrompt` / `buildSystemPrompt`): the prompt is sent with every
  model request and the language model is ~92% of a call-minute's cost. A prompt PR must state the
  token count before → after and A/B evidence on production's request shape.
- **Defaults that change paid calls or call behaviour**: speculation, hedging, pre-render,
  backchannels, barge-in/echo thresholds, turn-hold. Several were reverted after hurting
  production; say what was measured.
- **New providers or provider routing** (hosts, data retention): a new sub-processor must also be
  listed in `lokutor-platform` privacy/DPA — open the linked issue there (`touches:legal`).

## Commands and definition of done

```bash
make fmt && git diff --exit-code     # gofmt clean
make lint                            # go vet ./...
make test                            # go test -v -race ./...
```

Turno/ONNX tests skip without `libonnxruntime.so` (`ONNXRUNTIME_LIB_PATH`); if you change
`pkg/turno`, run them with the library and say so. A change is done when the commands pass, the
behaviour change has a test or a testbench result (named, with numbers), and the PR body lists
both. Never commit compiled binaries (the repo still carries `agent`, `main`,
`lokutor_orchestrator` from before — do not add more). `cmd/turnofixture` and `cmd/vadfixture`
exist untracked on some machines; they generate fixtures for the mobile Dart ports.

## Areas

| Label | Covers |
|---|---|
| `area:pipeline` | `pkg/orchestrator/managed_stream*.go`, orchestrator, conversation, Config |
| `area:turn-taking` | `pkg/turno/`, turn completion, silence nudge |
| `area:vad` | `vad.go`, `improved_vad.go` |
| `area:barge-in-echo` | echo correlation, spoken truth, barge-in hold/resume |
| `area:speculation` | speculative STT/LLM, pre-render |
| `area:prompt` | system prompt, context budget, response cache |
| `area:tools` | tool-call chaining and outcomes |
| `area:llm` | `pkg/providers/llm` (chain, hedge, providers, usage) |
| `area:stt` | `pkg/providers/stt` |
| `area:tts-audio` | `pkg/providers/tts`, sentence boundaries, backchannels, `pkg/audio`, prosody, noise |
| `area:observability` | latency checkpoints, token usage, turn logs |
| `area:tooling` | `cmd/`, CI, docs, fixtures |

## Issues and labels

Work is tracked in GitHub issues. Every change starts from an issue — if you find work that has
none, open one first (it can be two lines). The same label scheme is used in all Lokutor repos
(`lokutor_tts`, `lokutor-platform`, `lokutor-orchestrator`, `lokutor-voices`); only the `area:*`
labels differ per repo.

| Group | Labels | Rule |
|---|---|---|
| Type | `type:feature`, `type:bug`, `type:benchmark`, `type:perf`, `type:infra`, `type:security`, `type:research`, `type:docs`, `type:maintenance` | exactly one |
| Area | `area:*` (see this repo's list below) | one or more |
| Priority | `p0` (production down, money leaking, data at risk), `p1` (this week), `p2` (planned), `p3` (someday) | one; set by a human (an agent may suggest one in a comment) |
| Status | `status:triage`, `status:ready`, `status:in-progress`, `status:blocked`, `status:needs-human`, `status:needs-review` | exactly one while open |
| Risk | `touches:production`, `touches:money`, `touches:paired-values`, `touches:legal`, `cross-repo` | all that apply |
| Agents | `agent:ok` (an agent may take it end to end), `agent:human-only` | at most one |
| Origin | `from:real-call`, `from:customer`, `from:metrics` | optional |

What the types mean:

- `type:feature` — new capability or behaviour a user/operator can notice.
- `type:bug` — something does not do what it should. Include how to reproduce and the evidence.
- `type:benchmark` — a measurement whose output is a number someone will act on (capacity, cost,
  quality, latency, a model comparison). It must be reproducible: see the Benchmark template.
- `type:perf` — make something faster or cheaper; needs a before → after number.
- `type:infra` — deploy, CI, cluster, tooling, environments.
- `type:security` — auth, tenancy, secrets, exposure. Do not put exploit details in a public issue.
- `type:research` — a question to answer (spike); the deliverable is a FINDING comment, not code.
- `type:docs`, `type:maintenance` — documentation; refactors, dependency bumps, cleanup.

Status flow: `status:triage` (new) → `status:ready` (understood, can start) → `status:in-progress`
(claimed, see below) → `status:needs-review` (PR open) → closed by the merged PR. Side states:
`status:blocked` (waiting on something named in a comment) and `status:needs-human` (an agent
stopped because a decision or a gated action needs a person).

Issue templates (`.github/ISSUE_TEMPLATE/`): Bug, Feature, Benchmark / measurement, Infra / ops.

## Agent contract

Several people work in this repo, each possibly driving one or more coding agents. Agents
coordinate **only through GitHub** (issues, comments, PRs, branches) — never through local files,
chat or assumptions. These rules exist so two agents never do the same work, never undo each
other's, and a person can always see what an agent did and why.

### Identity

Agents act through their human's GitHub account (`gh` CLI). Every agent comment starts with a
header that says what wrote it and for whom:

```
**CLAIM** · agent: <tool/model> · for: @<github-user>
```

### Before starting

1. Read the whole issue and every comment.
2. Check nobody is already on it: the assignee, `status:in-progress`, open PRs
   (`gh pr list --search "<issue number>"`) and branches (`git branch -r | grep <number>`).
3. If the issue is `agent:human-only`, or has no `agent:ok` and touches production, money, legal
   or paired values: comment what you would do and stop (`status:needs-human`).

### Claiming

Assign the issue to your human, set `status:in-progress` (remove the previous status), and
comment:

```
**CLAIM** · agent: <tool/model> · for: @<github-user>
Branch: `<type>/<issue>-<slug>`
Plan: <one to three lines>
Out of scope: <what you will NOT touch>
```

One issue → one branch → one PR. Never commit to another agent's branch. An issue claimed by
someone else is theirs until they post a HANDOFF; if it looks abandoned (no comment for 3 working
days), ask in a comment and wait for the owner or a human to release it.

### Comments

Comment at milestones, not per commit. Every comment states facts with evidence (commands run and
their result, file:line, log lines, measurement files); anything not verified is marked
**unverified**. Never edit or delete someone else's comment — add a new one. Kinds:

| Kind | When | Must contain |
|---|---|---|
| **CLAIM** | taking the issue | branch, plan, out of scope |
| **PLAN** | the approach changed or needs agreement | options considered, the choice, why |
| **PROGRESS** | a meaningful step is done | done, verified (how), next |
| **FINDING** | learned something others need (root cause, a number, a surprise) | the fact, the evidence, what it changes |
| **BLOCKED** | cannot continue | what blocks, what is needed, from whom; set `status:blocked` or `status:needs-human` |
| **HANDOFF** | stopping before done | branch, last commit, what works, what is unverified, open questions, exact next step; unassign |
| **DONE** | PR is ready | PR link, what changed, how it was verified, follow-up issues opened |

Template for PROGRESS / HANDOFF / DONE:

```
**HANDOFF** · agent: <tool/model> · for: @<github-user>
Branch: `fix/42-overage-preprice` @ <short sha>
Done: ...
Verified: `<command>` → <result>
Not verified: ...
Open questions: ...
Next step: ...
```

### Scope and other agents

- Stay inside the issue. Anything else you notice becomes a new issue (`status:triage`, the right
  `type:`/`area:`, a link back) — not a drive-by change in your PR.
- If your change must touch files another open PR or claimed issue is changing, comment on both
  issues before you edit, and agree who goes first.
- Cross-repo work: one issue per repo, linked both ways (`Part of lokutor-ai/<repo>#<n>`),
  labelled `cross-repo`.
- Labels: agents set `type:`, `area:`, `status:`, risk and origin labels. Priority and
  `agent:ok` / `agent:human-only` are set by people.
- Agents never close issues by hand; the merged PR closes them (`Closes #n`). People close the rest.

## Branches, commits and pull requests

### Branches

`<type>/<issue>-<short-slug>`, e.g. `fix/42-overage-preprice`, `feat/57-canary-switch`,
`bench/61-f28k-capacity`. Types: `feat`, `fix`, `perf`, `bench`, `infra`, `sec`, `research`,
`docs`, `chore`. Branch from an up-to-date `main`.

### Commits

```
<type>(<area>): <outcome, imperative, ≤ 72 characters>

Why: the problem or the question, and how it showed up.
What: the change, and anything non-obvious about how.
Evidence: commands run, tests, measurements (file + date), before → after numbers.
Risk: what could break, and how to roll back if it is not obvious.

Refs #<issue>
```

- `type`: `feat`, `fix`, `perf`, `bench`, `infra`, `docs`, `refactor`, `chore`, `security`,
  `revert`. `area`: one of this repo's `area:` labels, without the prefix.
- The subject says what is now true, not what you did ("bill overage against the current plan",
  not "changed biller"). Numbers, file lists and reasoning go in the body.
- One logical change per commit; never mix formatting or renames with behaviour.
- **No AI attribution.** No `Co-Authored-By` lines for AI tools and no "Generated with …" footers
  in commits or PRs. Commits carry the author's own git identity.
- Never commit secrets, `.env*` files, credentials, generated data, large binaries or model
  weights (those live in LFS repos).

### Pull requests

- **Agents always work on a branch and open a PR; they never push to `main`.** People may push
  small, low-risk changes straight to `main`.
- Open the PR as a **draft** while work is in progress; mark it ready only when the definition of
  done (above) holds, then set `status:needs-review` on the issue and request review from the
  issue's human.
- Title: the same `type(area): outcome` format. Body: the PR template
  (`.github/pull_request_template.md`) — summary, `Closes #n`, verification with the exact commands
  and results, risk and rollout, rollback.
- **Agents never merge.** A person merges, preferably with **squash** (the PR title becomes the
  commit subject on `main`). Merging is a production action in repos where `main` deploys — see
  this repo's gates above.
- Review comments: answer each one in the thread (change made, or why not); do not resolve a
  reviewer's thread yourself.
