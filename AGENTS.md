# AGENTS.md

This document provides shared instructions for AI agents working in this repository. It applies to all responses, documents, and code comments. An `AGENTS.md` in a subdirectory may add instructions for its scope.

## 1. Searchable Writing

- Write repository documentation, code comments, and human-readable project text in English. Preserve exact identifiers, protocol values, and intentional non-English test data needed to verify behavior.
- Respond to the user in their preferred language. The English requirement applies to repository content, not conversational responses.
- Use standard Markdown that is easy to search. Prefer headings, paragraphs, numbered lists, and lists with ASCII hyphens (`-`).
- Do not use middle dots (U+00B7 or U+318D) or em dashes (U+2014). Connect items with commas or conjunctions, and use parentheses or separate sentences for additional explanation.
- Avoid decorative special characters and unnecessary Unicode separators. Symbols needed for technical explanations are allowed.
- Use consistent terminology for the same concept. Do not replace searchable terms with emoji, arbitrary abbreviations, or decorative characters.
- Write function names, types, configuration keys, environment variables, and file paths exactly as defined in the code, enclosed in backticks.
- Use stable, descriptive headings and filenames. Use lowercase English words and ASCII hyphens for ordinary document filenames, while retaining `README.md` and `AGENTS.md`.
- Prefer `rg` for text searches, falling back to `grep` when unavailable. For whole-word searches, use a command such as `rg -n -w 'keyword' docs/`.

## 2. Current State and Git Tracking

- Ordinary documentation and code comments describe the current code, configuration, and behavior. Do not describe unimplemented features or unverified behavior as established facts.
- Document only Git-tracked code, configuration, and shared procedures. General explanations and examples that anyone can reproduce are allowed.
- Create new documentation and examples as artifacts intended for Git tracking. The existence of a local file does not establish a shared repository behavior or procedure.
- Do not include personal usernames, personal absolute paths, developer-specific settings, local execution results, or procedures that depend on untracked files in shared documentation or comments.
- Use repository-relative paths. Express environment-specific values as placeholders or configuration keys defined in the repository.
- Comments explain implementation intent, constraints, and invariants that are difficult to understand from code alone. Do not record modification dates, authors, earlier implementations, or progress history.
- Record reasons for changes, before-and-after differences, and progress history in Git commit messages. Do not accumulate change logs in documentation or comments.
- Use `docs/history/` only when a separate historical document is necessary. Identify it as a historical record and link the recording date and relevant commit.
- Keep explanations needed to understand current behavior in ordinary documentation. Readers should not need to follow historical documents to understand the current implementation.

## 3. Documentation Structure and Maintenance

- Always organize documents and their sections top-down, from decisive information to supporting detail. Begin with the main conclusion, decision, required action, or governing rule and its scope. Follow with the rationale, procedures or examples, and implementation details or references.
- Put prerequisites and constraints that affect a reader's decision before the affected procedure or example. Keep procedural steps in execution order after stating the intended outcome and prerequisites.
- Introduce the overall behavior before package inventories, directory layouts, exhaustive configuration tables, or background. Within each section, state the rule before showing its examples. Apply this structure to reference documents and historical records as well as guides.
- When reviewing or updating existing documentation, check both document-level and section-level ordering. Move existing explanations where possible instead of adding duplicate summaries. Documentation indexes should orient readers before listing detailed links.
- Store documentation in `docs/`, organized by topic.
- Keep the root `README.md` as the project introduction and documentation entry point, and `AGENTS.md` as the agent instructions.
- The root `README.md` contains a short project overview, minimal getting-started guidance, and links to key documents. Maintain detailed feature behavior, configuration keys and examples, API specifications, operational procedures, and constraints in the relevant documents under `docs/`.
- When adding features or configuration, do not automatically append explanatory paragraphs to the root `README.md`. Update the detailed topic document first, then add or revise a relative link and a one-line description in `README.md` only if an entry point is needed.
- Do not duplicate detailed explanations or commands in the root `README.md`. When shortening it, check that removed content exists in detailed documentation, fill any gaps there, and link to it.
- Maintain the complete documentation index in `docs/README.md`. The root `README.md` links to that index and key documents for new readers without duplicating the entire index.
- When adding a document, create or update `docs/README.md` as the documentation index. Link to it from the root `README.md`.
- Create only necessary directories. The following is an example topic structure; do not create empty directories or placeholder documents in advance.

```text
docs/
  README.md
  architecture/
  development/
  operations/
  reference/
  history/
```

- Each document covers one main topic. Reduce duplicate explanations and provide relative links to the documents that maintain the details.
- When changing code, interfaces, configuration, or execution procedures, update affected documentation and comments in the same task.
- Revise or remove obsolete explanations, examples, and links instead of only appending new behavior.
- When moving or renaming a document, update the index and all links that reference it.
- Check that commands, configuration keys, file paths, and examples match the current repository. Do not present unverifiable content as definitive usage guidance.

## 4. Code-Based Reasoning

- Base judgments and inferences about implementation behavior primarily on actual code. Do not infer behavior solely from function names, comments, or existing documentation.
- First inspect relevant implementations, call paths, configuration, and tests. Follow dependencies into their actual implementations when necessary.
- When external libraries or systems affect a judgment, consult their official open-source implementations. For operating-system behavior, inspect the relevant kernel source code when needed.
- Check dependency versions, platforms, build options, and execution conditions, and use matching sources. Do not apply behavior from the latest source unchanged to a different version.
- When citing external evidence, include the repository URL, file path, symbol, and verified version, tag, or commit. Prefer links pinned to that revision.
- Verify important conclusions through implementation inspection and appropriate tests, minimal reproductions, or runtime observations. Do not generalize beyond what passing tests establish.
- Distinguish facts verified in code from inferences. When evidence is insufficient, state assumptions and unverified conditions and identify what needs further inspection.
- Link supporting evidence to current code locations. Maintain implementation history in commits or `docs/history/`.

## 5. Code Readability

- Use a single blank line between logical phases within a function, such as validation, initialization, processing, and response construction. Choose boundaries by purpose rather than statement type or line count.
- Keep closely related statements together, including a call and its error check, resource acquisition and its cleanup registration, and assignments that prepare the same operation.
- Keep short functions and cohesive blocks compact. Do not insert a blank line after every statement or condition, or before every return.
- Apply these rules when writing or editing functions. Use `gofmt` for Go formatting, and review logical grouping manually.

## 6. Agent Workflow

1. Inspect the applicable `AGENTS.md`, documentation index, relevant code, and Git status.
2. Identify differences between requirements and the current implementation, and inspect the code evidence needed for decisions.
3. Make the necessary changes. Preserve existing user changes and avoid edits outside the requested scope.
4. Update affected ordinary documentation and comments to reflect the current state. Check top-down ordering in the documents and sections being reviewed, and revise ordering where needed. Store historical records separately if needed.
5. Validate according to the change. For documentation-only changes, check top-down ordering, paths, links, terminology, formatting, and factual accuracy.
6. Report changes, evidence, validation results, and remaining unverified conditions concisely. Do not claim to have completed checks that were not run.
