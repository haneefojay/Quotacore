# CLAUDE.md

The operating manual for this repository is [AGENTS.md](AGENTS.md). Read that first; this file
exists because some harnesses look for `CLAUDE.md` and stop there.

If you only have room for four things, they are these.

1. **The specification is the product, and its questions are now closed.** There was no application
   code until [Gate 0](ROADMAP.md#gate-0--unblock-the-specification) closed on 2026-09-27.
   `IP-00` closed it and wrote no code; `IP-01` is the first phase that owns files here. No code
   outside a phase that is `IN PROGRESS` in
   [roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map) and owns the file. If asked for
   code that no phase owns, say so and offer the phase that should.
2. **Behaviour is stated once**, as a numbered `DR-nnn` in
   [domain-rules.md](docs/product/domain-rules.md). Reference the identifier; never restate the
   rule in another document, and never renumber one.
3. **Run `pwsh -File tools/check-docs.ps1` before and after every change** (`powershell -File
   tools/check-docs.ps1` is equivalent and needs no install on Windows). It checks links, anchors,
   identifier references, the counts in [docs/README.md](docs/README.md#inventory), and nine
   semantic checks that catch a contradiction between two documents. A failing check is a defect in
   the specification, not a warning.
4. **Record what you did in [CHANGELOG.md](CHANGELOG.md)** under `Unreleased`, and record what you
   deliberately did not decide in the open-questions register, with a default.

The two traps that have already caused a wrong edit: `T-01` is a named test while `T-1` is a tenant
state transition, and assumption identifiers are always two digits, so `A-02` resolves and the single-digit form does not.

Everything else — reading order by task, change recipes, writing conventions, the three claims
everything must prove, and the known traps in the current text — is in [AGENTS.md](AGENTS.md).
