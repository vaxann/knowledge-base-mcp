package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/blevesearch/bleve/v2/analysis/lang/ru"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
)

// SchemaVersion changes whenever the index layout changes; a mismatch
// triggers a rebuild.
const SchemaVersion = "1"

// Document is what gets indexed for one source.
type Document struct {
	Path     string    `json:"path"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Tags     []string  `json:"tags"`
	Folder   string    `json:"folder"`
	FM       string    `json:"fm"`   // frontmatter values as text
	FMKV     []string  `json:"fmkv"` // key=value pairs for exact filters
	Modified time.Time `json:"modified"`
	Source   string    `json:"source"` // note | attachment | embedding
}

// Filters narrow a search before ranking.
type Filters struct {
	Folder         string            `json:"folder,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	Frontmatter    map[string]string `json:"frontmatter,omitempty"`
	ModifiedAfter  *time.Time        `json:"modified_after,omitempty"`
	ModifiedBefore *time.Time        `json:"modified_before,omitempty"`
}

// Request is a ranked search request.
type Request struct {
	Query   string
	Limit   int
	Offset  int
	Filters Filters
}

// Hit is one ranked result.
type Hit struct {
	Path    string  `json:"path"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

// Result is a ranked search response.
type Result struct {
	Hits   []Hit  `json:"hits"`
	Total  uint64 `json:"total"`
	TookMs int64  `json:"took_ms"`
}

// Stamp records what the index was built from.
type Stamp struct {
	Schema    string    `json:"schema"`
	VaultPath string    `json:"vault_path"`
	Revision  string    `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	Languages []string  `json:"languages"`
}

// Indexer is the full-text index abstraction.
type Indexer interface {
	Upsert(ctx context.Context, doc Document) error
	UpsertMany(ctx context.Context, docs []Document) error
	Delete(ctx context.Context, path string) error
	Search(ctx context.Context, req Request) (Result, error)
	Count() (uint64, error)
	Stamp() (Stamp, bool, error)
	SetStamp(Stamp) error
	Analyze(text string) []string
	Close() error
}

// BleveIndex implements Indexer with Bleve.
type BleveIndex struct {
	idx   bleve.Index
	dir   string
	langs []string
}

const analyzerName = "kb"

func buildMapping(langs []string) (mapping.IndexMapping, error) {
	im := bleve.NewIndexMapping()
	filters := []any{lowercase.Name}
	for _, l := range langs {
		switch l {
		case "ru":
			filters = append(filters, ru.SnowballStemmerName)
		case "en":
			filters = append(filters, en.PossessiveName, en.SnowballStemmerName)
		default:
			return nil, fmt.Errorf("unsupported search language %q", l)
		}
	}
	if err := im.AddCustomAnalyzer(analyzerName, map[string]any{
		"type": custom.Name, "tokenizer": unicode.Name, "token_filters": filters,
	}); err != nil {
		return nil, err
	}
	text := bleve.NewTextFieldMapping()
	text.Analyzer = analyzerName
	text.Store = true
	kw := bleve.NewKeywordFieldMapping()
	kw.Store = true
	kwNoStore := bleve.NewKeywordFieldMapping()
	kwNoStore.Store = false
	dt := bleve.NewDateTimeFieldMapping()
	doc := bleve.NewDocumentStaticMapping()
	doc.AddFieldMappingsAt("path", kw)
	doc.AddFieldMappingsAt("title", text)
	doc.AddFieldMappingsAt("body", text)
	doc.AddFieldMappingsAt("fm", text)
	doc.AddFieldMappingsAt("tags", kw)
	doc.AddFieldMappingsAt("folder", kwNoStore)
	doc.AddFieldMappingsAt("fmkv", kwNoStore)
	doc.AddFieldMappingsAt("source", kwNoStore)
	doc.AddFieldMappingsAt("modified", dt)
	im.DefaultMapping = doc
	im.DefaultAnalyzer = analyzerName
	return im, nil
}

// OpenBleve opens or creates the index at dir. When reset is true the
// existing index is discarded.
func OpenBleve(dir string, langs []string, reset bool) (*BleveIndex, error) {
	if reset {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
	}
	idx, err := bleve.Open(dir)
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		m, merr := buildMapping(langs)
		if merr != nil {
			return nil, merr
		}
		if err := os.MkdirAll(path.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		idx, err = bleve.New(dir, m)
	}
	if err != nil {
		return nil, err
	}
	return &BleveIndex{idx: idx, dir: dir, langs: langs}, nil
}

// NewMemoryBleve creates an in-memory index (tests, benchmarks).
func NewMemoryBleve(langs []string) (*BleveIndex, error) {
	m, err := buildMapping(langs)
	if err != nil {
		return nil, err
	}
	idx, err := bleve.NewMemOnly(m)
	if err != nil {
		return nil, err
	}
	return &BleveIndex{idx: idx, langs: langs}, nil
}

// Close releases the index.
func (b *BleveIndex) Close() error { return b.idx.Close() }

// Upsert indexes a document keyed by path.
func (b *BleveIndex) Upsert(_ context.Context, doc Document) error {
	if doc.Source == "" {
		doc.Source = "note"
	}
	doc.Folder = path.Dir(doc.Path)
	if doc.Folder == "." {
		doc.Folder = ""
	}
	return b.idx.Index(doc.Path, doc)
}

// UpsertMany indexes documents in one batch.
func (b *BleveIndex) UpsertMany(_ context.Context, docs []Document) error {
	batch := b.idx.NewBatch()
	for _, doc := range docs {
		if doc.Source == "" {
			doc.Source = "note"
		}
		doc.Folder = path.Dir(doc.Path)
		if doc.Folder == "." {
			doc.Folder = ""
		}
		if err := batch.Index(doc.Path, doc); err != nil {
			return err
		}
	}
	return b.idx.Batch(batch)
}

// Delete removes a document.
func (b *BleveIndex) Delete(_ context.Context, p string) error { return b.idx.Delete(p) }

// Count returns the number of indexed documents.
func (b *BleveIndex) Count() (uint64, error) { return b.idx.DocCount() }

const stampKey = "kb.stamp"

// Stamp reads the build stamp.
func (b *BleveIndex) Stamp() (Stamp, bool, error) {
	raw, err := b.idx.GetInternal([]byte(stampKey))
	if err != nil {
		return Stamp{}, false, err
	}
	if len(raw) == 0 {
		return Stamp{}, false, nil
	}
	var s Stamp
	if err := json.Unmarshal(raw, &s); err != nil {
		return Stamp{}, false, err
	}
	return s, true, nil
}

// SetStamp writes the build stamp.
func (b *BleveIndex) SetStamp(s Stamp) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return b.idx.SetInternal([]byte(stampKey), raw)
}

// Analyze tokenizes text with the index analyzer (stemmed, lower-cased).
func (b *BleveIndex) Analyze(text string) []string {
	an := b.idx.Mapping().AnalyzerNamed(analyzerName)
	if an == nil {
		return strings.Fields(strings.ToLower(text))
	}
	toks := an.Analyze([]byte(text))
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		out = append(out, string(t.Term))
	}
	return out
}

// Search runs a ranked query.
func (b *BleveIndex) Search(ctx context.Context, req Request) (Result, error) {
	if req.Limit <= 0 {
		req.Limit = 10
	}
	q := b.buildQuery(req)
	sr := bleve.NewSearchRequestOptions(q, req.Limit, req.Offset, false)
	sr.Fields = []string{"path", "title"}
	sr.Highlight = bleve.NewHighlightWithStyle("html")
	sr.Highlight.AddField("body")
	sr.Highlight.AddField("title")
	res, err := b.idx.SearchInContext(ctx, sr)
	if err != nil {
		return Result{}, err
	}
	out := Result{Total: res.Total, TookMs: res.Took.Milliseconds()}
	for _, h := range res.Hits {
		hit := Hit{Path: h.ID, Score: h.Score}
		if t, ok := h.Fields["title"].(string); ok {
			hit.Title = t
		}
		hit.Snippet = snippetOf(h.Fragments)
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

func snippetOf(frags map[string][]string) string {
	parts := frags["body"]
	if len(parts) == 0 {
		parts = frags["title"]
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	s := strings.Join(parts, " … ")
	s = strings.ReplaceAll(s, "<mark>", "**")
	s = strings.ReplaceAll(s, "</mark>", "**")
	s = html.UnescapeString(s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func (b *BleveIndex) buildQuery(req Request) query.Query {
	conj := bleve.NewConjunctionQuery()
	terms := parseQueryTerms(req.Query)
	if len(terms) == 0 {
		conj.AddQuery(bleve.NewMatchAllQuery())
	}
	for _, t := range terms {
		disj := bleve.NewDisjunctionQuery()
		switch {
		case t.phrase:
			for _, f := range []string{"title", "body", "fm"} {
				pq := bleve.NewMatchPhraseQuery(t.text)
				pq.SetField(f)
				pq.Analyzer = analyzerName
				if f == "title" {
					pq.SetBoost(3)
				}
				disj.AddQuery(pq)
			}
		case t.prefix:
			for _, f := range []string{"title", "body", "fm"} {
				pq := bleve.NewPrefixQuery(strings.ToLower(t.text))
				pq.SetField(f)
				if f == "title" {
					pq.SetBoost(3)
				}
				disj.AddQuery(pq)
			}
		default:
			for _, f := range []string{"title", "body", "fm"} {
				mq := bleve.NewMatchQuery(t.text)
				mq.SetField(f)
				mq.Analyzer = analyzerName
				if f == "title" {
					mq.SetBoost(3)
				}
				disj.AddQuery(mq)
			}
			tq := bleve.NewTermQuery(strings.TrimPrefix(t.text, "#"))
			tq.SetField("tags")
			tq.SetBoost(2)
			disj.AddQuery(tq)
		}
		conj.AddQuery(disj)
	}
	f := req.Filters
	if f.Folder != "" {
		folder := strings.Trim(f.Folder, "/")
		pq := bleve.NewPrefixQuery(folder + "/")
		pq.SetField("path")
		conj.AddQuery(pq)
	}
	for _, tag := range f.Tags {
		tq := bleve.NewTermQuery(strings.TrimPrefix(tag, "#"))
		tq.SetField("tags")
		conj.AddQuery(tq)
	}
	for k, v := range f.Frontmatter {
		tq := bleve.NewTermQuery(k + "=" + v)
		tq.SetField("fmkv")
		conj.AddQuery(tq)
	}
	if f.ModifiedAfter != nil || f.ModifiedBefore != nil {
		start, end := time.Time{}, time.Time{}
		if f.ModifiedAfter != nil {
			start = *f.ModifiedAfter
		}
		if f.ModifiedBefore != nil {
			end = *f.ModifiedBefore
		}
		dq := bleve.NewDateRangeQuery(start, end)
		dq.SetField("modified")
		conj.AddQuery(dq)
	}
	return conj
}

type queryTerm struct {
	text   string
	phrase bool
	prefix bool
}

func parseQueryTerms(q string) []queryTerm {
	var out []queryTerm
	q = strings.TrimSpace(q)
	for q != "" {
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			if end < 0 {
				q = q[1:]
				continue
			}
			if p := strings.TrimSpace(q[1 : end+1]); p != "" {
				out = append(out, queryTerm{text: p, phrase: true})
			}
			q = strings.TrimSpace(q[end+2:])
			continue
		}
		sp := strings.IndexAny(q, " \t\n")
		word := q
		if sp >= 0 {
			word, q = q[:sp], strings.TrimSpace(q[sp+1:])
		} else {
			q = ""
		}
		if strings.HasSuffix(word, "*") && len(word) > 1 {
			out = append(out, queryTerm{text: strings.TrimSuffix(word, "*"), prefix: true})
		} else if word != "" {
			out = append(out, queryTerm{text: word})
		}
	}
	return out
}

// DocumentOf converts a note into an index document.
func DocumentOf(m *NoteMeta, body string) Document {
	d := Document{Path: m.Path, Title: m.Title, Body: body, Tags: m.Tags, Modified: m.ModTime, Source: "note"}
	var fmText []string
	for k, v := range m.Frontmatter {
		for _, s := range scalarStrings(v) {
			fmText = append(fmText, s)
			d.FMKV = append(d.FMKV, k+"="+s)
		}
	}
	d.FM = strings.Join(fmText, "\n")
	return d
}

func scalarStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, scalarStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, scalarStrings(e)...)
		}
		return out
	case time.Time:
		return []string{x.Format("2006-01-02")}
	default:
		return []string{fmt.Sprint(x)}
	}
}
