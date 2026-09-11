## Purpose

Lets clients read notes and navigate the vault: content with parsed metadata, sections, listings, tags, links and backlinks, and MCP resources.

## ADDED Requirements

### Requirement: Get a note by path
The server SHALL provide `kb_get_note(path)` returning the note's raw content, parsed frontmatter, body, title (frontmatter `title` or first H1 or file stem), headings outline, tags, outgoing links (resolved paths where possible), file size, modification time, last commit hash and date, and an `etag` (content hash) for optimistic concurrency.

#### Scenario: Existing note
- **WHEN** a client requests an existing note
- **THEN** all fields above are returned and `etag` changes if and only if the content changes

#### Scenario: Missing note
- **WHEN** a client requests a path that does not exist or is excluded
- **THEN** the call fails with code `not_found`

### Requirement: Get a section by heading
The server SHALL provide `kb_get_section(path, heading)` returning the text under the first heading matching `heading` (case-insensitive, up to but excluding the next heading of the same or higher level), together with the heading level and its byte range.

#### Scenario: Section exists
- **WHEN** a note contains `## History` followed by two paragraphs and then `## Notes`
- **THEN** the call returns exactly those two paragraphs

#### Scenario: Section missing
- **WHEN** no heading matches
- **THEN** the call fails with code `not_found`

### Requirement: List notes and folders
The server SHALL provide `kb_list(folder, recursive, glob, limit, cursor)` returning entries (path, kind, title for notes, size, modified time) sorted by path, with cursor-based pagination and an optional glob filter on the relative path. `folder` defaults to the vault root.

#### Scenario: Recursive listing with glob
- **WHEN** a client lists `Projects` recursively with glob `**/*.md`
- **THEN** only Markdown notes under `Projects` are returned, in path order

#### Scenario: Pagination
- **WHEN** a listing exceeds `limit`
- **THEN** the response includes `next_cursor`, and passing it returns the remaining entries without duplicates

### Requirement: Backlinks
The server SHALL provide `kb_backlinks(path)` returning every note that links to the target through a wikilink (`[[Name]]`, `[[Name|alias]]`, `[[Name#heading]]`, `[[folder/Name]]`) or a relative Markdown link, resolving links with Obsidian's shortest-unique-path rule, and including the line context of each link.

#### Scenario: Wikilink by basename
- **WHEN** note A contains `[[Target|see this]]` and `Target.md` is unique in the vault
- **THEN** backlinks of `Target.md` include A with the context line

#### Scenario: Ambiguous basename
- **WHEN** two notes share the basename `Meeting` and note B links `[[Work/Meeting]]`
- **THEN** only `Work/Meeting.md` lists B as a backlink

### Requirement: Tags
The server SHALL provide `kb_tags(prefix)` returning tags collected from frontmatter `tags` (string or list) and inline `#tag` tokens in note bodies (excluding code blocks), each with the number of notes using it, sorted by count then name.

#### Scenario: Tag counts
- **WHEN** three notes carry `#project` inline and one carries `tags: [project]` in frontmatter
- **THEN** `project` is reported with a count of 4

### Requirement: Notes exposed as MCP resources
The server SHALL expose each note as the resource `kb://note/<path>` with media type `text/markdown` and each folder as `kb://folder/<path>` with a JSON listing, and SHALL support resource listing and templates so clients can discover them.

#### Scenario: Read a note resource
- **WHEN** a client reads `kb://note/Ideas/Idea.md`
- **THEN** the raw Markdown content is returned
