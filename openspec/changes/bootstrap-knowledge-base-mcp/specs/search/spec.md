## Purpose

Gives clients fast, ranked search and structured queries over the vault, plus retrieval bundles sized for a language model's context, backed by an always-fresh index.

## ADDED Requirements

### Requirement: Full-text search
The server SHALL provide `kb_search(query, limit, offset, filters)` over note titles, bodies, tags and frontmatter values. Matching MUST be case-insensitive and MUST apply stemming for Russian and English so that inflected forms match. Results SHALL be ranked by relevance with title matches boosted, and each result SHALL include path, title, score, and a snippet with highlighted matches. Quoted phrases SHALL match exactly; a trailing `*` SHALL match by prefix.

#### Scenario: Inflected Russian query
- **WHEN** a note contains the word `паспорта` and the client searches `паспорт`
- **THEN** the note is returned with the matching term highlighted in the snippet

#### Scenario: Exact phrase
- **WHEN** the client searches `"annual report"`
- **THEN** only notes containing that exact phrase are returned

#### Scenario: Title boost
- **WHEN** one note is titled `Budget 2026` and another only mentions `budget 2026` in its body
- **THEN** the titled note ranks first

### Requirement: Search filters
`kb_search` SHALL accept filters for folder prefix, tags (all must match), frontmatter field equality, and modified-after / modified-before dates, applied before ranking.

#### Scenario: Folder and tag filter
- **WHEN** the client searches `invoice` with folder `Finance` and tag `2026`
- **THEN** only notes under `Finance/` tagged `2026` and matching `invoice` are returned

### Requirement: Structured metadata query
The server SHALL provide `kb_query(where, select, sort, limit)` evaluating predicates over frontmatter fields and note metadata (path, folder, tags, modified): operators `eq`, `ne`, `in`, `contains`, `exists`, `gt`, `gte`, `lt`, `lte` (dates and numbers compared natively), combined with `and`/`or`/`not`. The result SHALL be a table of the selected fields per matching note.

#### Scenario: Filter by type and person
- **WHEN** the client queries `type eq "document" and persons contains "Alice"`, selecting `path, expires`
- **THEN** every returned row has `type: document` and `Alice` in `persons`, and only the two selected fields plus `path` are returned

#### Scenario: Date comparison
- **WHEN** the client queries `expires lt 2026-12-31`
- **THEN** only notes whose `expires` frontmatter date is earlier are returned, and notes without the field are excluded

### Requirement: Context bundle for retrieval-augmented answers
The server SHALL provide `kb_context(query, max_chars, filters)` returning the most relevant note sections (split by heading) concatenated in relevance order, each prefixed with its source path and heading, and truncated so the total does not exceed `max_chars`. The response SHALL list the sources used.

#### Scenario: Bundle within budget
- **WHEN** the client asks for `max_chars` 8000 on a query matching twenty sections
- **THEN** the returned text is at most 8000 characters, contains whole sections only (except the last, which may be truncated with a marker), and `sources` lists each included path

### Requirement: Quick open by title or path
The server SHALL provide `kb_quick_open(text, limit)` performing prefix and fuzzy matching on note titles and paths, returning candidates ordered by match quality, intended for resolving a note a client refers to by an approximate name.

#### Scenario: Fuzzy title
- **WHEN** the client passes `budgt 26`
- **THEN** `Budget 2026.md` is the first candidate

### Requirement: Search latency
With a warm index, `kb_search` and `kb_quick_open` SHALL complete with p95 ≤ 100 ms for vaults up to 5,000 notes and p95 ≤ 300 ms up to 20,000 notes, measured on the benchmark fixture on commodity hardware.

#### Scenario: Benchmark
- **WHEN** the benchmark suite runs against the generated 5,000-note vault
- **THEN** the recorded p95 for `kb_search` is at most 100 ms

### Requirement: Index lifecycle and freshness
The index SHALL be stored outside the vault (or in a git-ignored location) and SHALL be rebuilt on startup when it is absent, when its schema version differs, or when its recorded vault revision does not match the current Git `HEAD`. Changes made through write tools SHALL be indexed synchronously. Changes made outside the server (desktop editor, `git pull`) SHALL be reflected within 2 seconds. `kb_reindex` SHALL force a full rebuild, and `kb_info` SHALL report the indexed note count and the time of the last index update.

#### Scenario: External edit
- **WHEN** a file in the vault is modified by another program
- **THEN** within 2 seconds a search for its new content returns it

#### Scenario: Remote change pulled
- **WHEN** an automatic pull brings a commit adding a note
- **THEN** the note becomes searchable without a manual reindex

#### Scenario: Forced rebuild
- **WHEN** a client calls `kb_reindex`
- **THEN** the index is rebuilt from scratch and the note count in `kb_info` matches the number of visible notes
