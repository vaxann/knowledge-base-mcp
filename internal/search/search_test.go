package search

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

type env struct {
	v   *vault.Vault
	cat *Catalog
	idx *BleveIndex
}

func loadFixture(t testing.TB) *env {
	t.Helper()
	v, err := vault.Open(testutil.CopyFixture(t), []string{"Private/**"})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := NewMemoryBleve([]string{"ru", "en"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cat := NewCatalog()
	var metas []*NoteMeta
	var files []vault.Entry
	err = v.Walk("", func(rel string, info fs.FileInfo) error {
		if !vault.IsMarkdown(rel) {
			files = append(files, vault.Entry{Path: rel, Kind: "attachment", Size: info.Size()})
			return nil
		}
		n, err := v.ReadNote(rel)
		if err != nil {
			return err
		}
		m := MetaOf(n)
		metas = append(metas, m)
		return idx.Upsert(context.Background(), DocumentOf(m, string(n.Body)))
	})
	if err != nil {
		t.Fatal(err)
	}
	cat.Replace(metas, files)
	return &env{v: v, cat: cat, idx: idx}
}

func paths(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}

func TestSearchStemmingAndHighlight(t *testing.T) {
	e := loadFixture(t)
	res, err := e.idx.Search(context.Background(), Request{Query: "паспорт"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "Projects/Beta.md" {
		t.Fatalf("Russian stemming failed: %v", paths(res.Hits))
	}
	if !strings.Contains(res.Hits[0].Snippet, "**") || !strings.Contains(strings.ToLower(res.Hits[0].Snippet), "паспорт") {
		t.Errorf("snippet without highlight: %q", res.Hits[0].Snippet)
	}
	res, _ = e.idx.Search(context.Background(), Request{Query: "linking"})
	if len(res.Hits) == 0 || res.Hits[0].Path != "Projects/Beta.md" {
		t.Errorf("English stemming failed for 'linking' -> 'Links': %v", paths(res.Hits))
	}
	res, _ = e.idx.Search(context.Background(), Request{Query: "kicked"})
	if len(res.Hits) != 1 || res.Hits[0].Path != "Projects/Alpha.md" {
		t.Errorf("plain term: %v", paths(res.Hits))
	}
}

func TestSearchPhrasePrefixTitleBoost(t *testing.T) {
	e := loadFixture(t)
	ctx := context.Background()
	res, _ := e.idx.Search(ctx, Request{Query: `"annual report"`})
	got := paths(res.Hits)
	sort.Strings(got)
	if strings.Join(got, ",") != "Ideas/Budget 2026.md,Notes/Meeting.md" {
		t.Errorf("phrase search: %v", got)
	}
	res, _ = e.idx.Search(ctx, Request{Query: `"report annual"`})
	if len(res.Hits) != 0 {
		t.Errorf("phrase order must matter: %v", paths(res.Hits))
	}
	res, _ = e.idx.Search(ctx, Request{Query: "budg*"})
	if len(res.Hits) < 2 {
		t.Errorf("prefix search: %v", paths(res.Hits))
	}
	res, _ = e.idx.Search(ctx, Request{Query: "budget 2026"})
	if len(res.Hits) < 2 || res.Hits[0].Path != "Ideas/Budget 2026.md" {
		t.Errorf("title boost: %v", paths(res.Hits))
	}
}

func TestSearchFilters(t *testing.T) {
	e := loadFixture(t)
	ctx := context.Background()
	res, _ := e.idx.Search(ctx, Request{Query: "meeting", Filters: Filters{Folder: "Work"}})
	if strings.Join(paths(res.Hits), ",") != "Work/Meeting.md" {
		t.Errorf("folder filter: %v", paths(res.Hits))
	}
	res, _ = e.idx.Search(ctx, Request{Filters: Filters{Tags: []string{"project"}}})
	got := paths(res.Hits)
	sort.Strings(got)
	if strings.Join(got, ",") != "Projects/Alpha.md,Projects/Beta.md" {
		t.Errorf("tag filter: %v", got)
	}
	res, _ = e.idx.Search(ctx, Request{Filters: Filters{Frontmatter: map[string]string{"status": "archive"}}})
	if strings.Join(paths(res.Hits), ",") != "Projects/Beta.md" {
		t.Errorf("frontmatter filter: %v", paths(res.Hits))
	}
	past := time.Now().Add(-time.Hour)
	res, _ = e.idx.Search(ctx, Request{Query: "alpha", Filters: Filters{ModifiedAfter: &past}})
	if len(res.Hits) == 0 {
		t.Error("modified_after filter dropped everything")
	}
	future := time.Now().Add(time.Hour)
	res, _ = e.idx.Search(ctx, Request{Query: "alpha", Filters: Filters{ModifiedAfter: &future}})
	if len(res.Hits) != 0 {
		t.Error("modified_after in the future should match nothing")
	}
}

func TestStampAndDelete(t *testing.T) {
	e := loadFixture(t)
	ctx := context.Background()
	if _, ok, _ := e.idx.Stamp(); ok {
		t.Fatal("fresh index should have no stamp")
	}
	if err := e.idx.SetStamp(Stamp{Schema: SchemaVersion, Revision: "abc"}); err != nil {
		t.Fatal(err)
	}
	st, ok, _ := e.idx.Stamp()
	if !ok || st.Revision != "abc" {
		t.Errorf("stamp round-trip: %+v", st)
	}
	if err := e.idx.Delete(ctx, "Projects/Beta.md"); err != nil {
		t.Fatal(err)
	}
	res, _ := e.idx.Search(ctx, Request{Query: "паспорт"})
	if len(res.Hits) != 0 {
		t.Error("deleted document still found")
	}
	n, _ := e.idx.Count()
	if n != 7 {
		t.Errorf("count = %d", n)
	}
}

func TestCatalogBacklinksTagsQuickOpen(t *testing.T) {
	e := loadFixture(t)
	bl := e.cat.Backlinks("Projects/Alpha.md")
	var srcs []string
	for _, b := range bl {
		srcs = append(srcs, b.Source)
	}
	sort.Strings(srcs)
	if strings.Join(srcs, ",") != "Contacts/Alice.md,Notes/Meeting.md,Projects/Beta.md,README.md" {
		t.Errorf("backlinks: %v", srcs)
	}
	bl = e.cat.Backlinks("Notes/Meeting.md")
	if len(bl) != 2 { // [[Notes/Meeting|the meeting]] and ../Notes/Meeting.md, both from Alpha
		t.Errorf("Notes/Meeting backlinks: %+v", bl)
	}
	bl = e.cat.Backlinks("Work/Meeting.md")
	if len(bl) != 1 || bl[0].Source != "Projects/Alpha.md" || !strings.Contains(bl[0].Context, "[[Work/Meeting]]") {
		t.Errorf("Work/Meeting backlinks: %+v", bl)
	}
	tags := e.cat.Tags("")
	if tags[0].Tag != "project" || tags[0].Count != 2 {
		t.Errorf("tags: %+v", tags)
	}
	if f := e.cat.Tags("fin"); len(f) != 1 || f[0].Tag != "finance/planning" {
		t.Errorf("tag prefix: %+v", f)
	}
	c := e.cat.QuickOpen("budgt 26", 5)
	if len(c) == 0 || c[0].Path != "Ideas/Budget 2026.md" {
		t.Errorf("quick open fuzzy: %+v", c)
	}
	c = e.cat.QuickOpen("meeting", 5)
	if len(c) != 2 {
		t.Errorf("quick open exact: %+v", c)
	}
	c = e.cat.QuickOpen("work meet", 5)
	if len(c) == 0 || c[0].Path != "Work/Meeting.md" {
		t.Errorf("quick open path token: %+v", c)
	}
}

func TestCatalogUpsertDelete(t *testing.T) {
	e := loadFixture(t)
	n := vault.Parse("Notes/New.md", []byte("# New\n\nLinks to [[Alpha]] and [[Beta]].\n"))
	e.cat.Upsert(MetaOf(n))
	if bl := e.cat.Backlinks("Projects/Beta.md"); len(bl) != 3 {
		t.Errorf("backlinks after upsert: %+v", bl)
	}
	e.cat.Delete("Notes/New.md")
	if bl := e.cat.Backlinks("Projects/Beta.md"); len(bl) != 2 {
		t.Errorf("backlinks after delete: %+v", bl)
	}
	if _, ok := e.cat.Get("Notes/New.md"); ok {
		t.Error("note still in catalog")
	}
}

func TestQueryLanguage(t *testing.T) {
	e := loadFixture(t)
	cases := []struct {
		where string
		want  string
	}{
		{`type eq "project"`, "Projects/Alpha.md,Projects/Beta.md"},
		{`type eq "project" and status eq "active"`, "Projects/Alpha.md"},
		{`type = "project" and not status = "active"`, "Projects/Beta.md"},
		{`persons contains "alice"`, "Projects/Alpha.md"},
		{`tags contains "contact" or type eq "meeting"`, "Contacts/Alice.md,Work/Meeting.md"},
		{`expires lt 2026-12-31`, "Ideas/Budget 2026.md"},
		{`expires gt 2026-12-31`, ""},
		{`value gte 8`, "Ideas/Budget 2026.md"},
		{`priority exists`, "Projects/Alpha.md"},
		{`type in ["contact", "meeting"]`, "Contacts/Alice.md,Work/Meeting.md"},
		{`folder eq "Projects" and started >= 2026-01-01`, "Projects/Alpha.md"},
		{`(type eq "idea" and value exists) or name eq "Alice"`, "Contacts/Alice.md,Ideas/Budget 2026.md"},
		{`path contains "meeting"`, "Notes/Meeting.md,Work/Meeting.md"},
	}
	for _, c := range cases {
		rows, err := e.cat.RunQuery(QueryRequest{Where: c.where, Select: []string{"path"}})
		if err != nil {
			t.Errorf("%s: %v", c.where, err)
			continue
		}
		var got []string
		for _, r := range rows {
			got = append(got, r["path"].(string))
		}
		sort.Strings(got)
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: got %v want %s", c.where, got, c.want)
		}
	}
	rows, err := e.cat.RunQuery(QueryRequest{Where: `type exists`, Select: []string{"type"}, Sort: "type desc", Limit: 2})
	if err != nil || len(rows) != 2 || rows[0]["type"] != "project" {
		t.Errorf("sort/limit/select: %v %v", rows, err)
	}
	if _, err := e.cat.RunQuery(QueryRequest{Where: `type frobnicate "x"`}); err == nil {
		t.Error("unknown operator must fail")
	}
	rows, _ = e.cat.RunQuery(QueryRequest{Where: `type eq "contact"`})
	if rows[0]["email"] != "alice@example.com" || rows[0]["title"] != "Alice" {
		t.Errorf("default select should include frontmatter: %v", rows[0])
	}
}

func TestGrep(t *testing.T) {
	e := loadFixture(t)
	ctx := context.Background()
	res, err := Grep(ctx, e.v, GrepRequest{Pattern: "[[Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range res.Files {
		files = append(files, f.Path)
	}
	if strings.Join(files, ",") != "Contacts/Alice.md,Notes/Meeting.md,Projects/Alpha.md,Projects/Beta.md,README.md" {
		t.Errorf("grep files: %v", files)
	}
	for _, f := range res.Files {
		if f.Path == "Projects/Beta.md" && len(f.Matches) != 2 {
			t.Errorf("code block lines must be included: %+v", f.Matches)
		}
	}
	res, _ = Grep(ctx, e.v, GrepRequest{Pattern: `(?i)budget \d{4}`, Regex: true, Glob: "Ideas/**", ContextLines: 1})
	if len(res.Files) != 1 || res.Files[0].Path != "Ideas/Budget 2026.md" || len(res.Files[0].Matches) != 2 || res.Files[0].Matches[1].Line != 10 || len(res.Files[0].Matches[0].Before) != 1 {
		t.Errorf("regex+glob: %+v", res.Files)
	}
	res, _ = Grep(ctx, e.v, GrepRequest{Pattern: "ALPHA", CaseSensitive: true})
	if len(res.Files) != 0 {
		t.Errorf("case sensitive: %+v", res.Files)
	}
	res, _ = Grep(ctx, e.v, GrepRequest{Pattern: "secret"})
	if len(res.Files) != 0 {
		t.Error("excluded folder must not be grepped")
	}
	res, _ = Grep(ctx, e.v, GrepRequest{Pattern: "e", Limit: 3})
	if res.Total != 3 || !res.Truncated {
		t.Errorf("limit: total=%d truncated=%v", res.Total, res.Truncated)
	}
	if _, err := Grep(ctx, e.v, GrepRequest{Pattern: "(", Regex: true}); err == nil {
		t.Error("bad regex must fail")
	}
}

func TestContextBundle(t *testing.T) {
	e := loadFixture(t)
	res, err := BuildContext(context.Background(), e.idx, e.v, ContextRequest{Query: "budget 2026", MaxChars: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > 300 || res.Chunks == 0 || len(res.Sources) == 0 {
		t.Fatalf("bundle: len=%d chunks=%d sources=%v", len(res.Text), res.Chunks, res.Sources)
	}
	if !strings.HasPrefix(res.Text, "## ") || !strings.Contains(res.Text, "udget") {
		t.Errorf("bundle text:\n%s", res.Text)
	}
	small, _ := BuildContext(context.Background(), e.idx, e.v, ContextRequest{Query: "alpha", MaxChars: 60})
	if len(small.Text) > 60 || !strings.Contains(small.Text, "[truncated]") {
		t.Errorf("truncation: %q", small.Text)
	}
}

func TestOpenBleveInExistingEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := OpenBleve(dir, []string{"ru", "en"}, false)
	if err != nil {
		t.Fatalf("open in empty dir: %v", err)
	}
	if err := idx.Upsert(context.Background(), Document{Path: "a.md", Title: "A", Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	idx, err = OpenBleve(dir, []string{"ru", "en"}, false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n, _ := idx.Count(); n != 1 {
		t.Errorf("count after reopen = %d", n)
	}
	_ = idx.Close()
	// A leftover empty bleve subdirectory must not block startup either.
	if err := os.RemoveAll(filepath.Join(dir, "bleve")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bleve"), 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err = OpenBleve(dir, []string{"ru", "en"}, false)
	if err != nil {
		t.Fatalf("open with empty bleve subdir: %v", err)
	}
	_ = idx.Close()
}

// --- benchmark over a generated vault ---

var words = strings.Fields("проект документ паспорт бюджет встреча отчёт задача идея контакт клиент договор счёт виза страховка школа поездка project document budget meeting report task idea contact client contract invoice visa insurance school trip alpha beta gamma delta zebra")

func generateVault(b testing.TB, n int) *vault.Vault {
	b.Helper()
	root := b.TempDir()
	r := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		folder := fmt.Sprintf("F%02d", i%25)
		var body strings.Builder
		fmt.Fprintf(&body, "---\ntype: t%d\nstatus: active\ntags: [g%d]\n---\n# Note %d %s\n\n", i%7, i%13, i, words[r.Intn(len(words))])
		for s := 0; s < 5; s++ {
			fmt.Fprintf(&body, "## Section %d\n\n", s)
			for w := 0; w < 80; w++ {
				body.WriteString(words[r.Intn(len(words))])
				body.WriteByte(' ')
			}
			body.WriteString("\n\n")
		}
		p := filepath.Join(root, folder, fmt.Sprintf("note-%05d.md", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body.String()), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	v, err := vault.Open(root, nil)
	if err != nil {
		b.Fatal(err)
	}
	return v
}

func indexVault(b testing.TB, v *vault.Vault) (*BleveIndex, *Catalog) {
	b.Helper()
	idx, err := OpenBleve(filepath.Join(b.TempDir(), "idx"), []string{"ru", "en"}, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	cat := NewCatalog()
	var metas []*NoteMeta
	batch := idx.idx.NewBatch()
	err = v.Walk("", func(rel string, _ fs.FileInfo) error {
		n, err := v.ReadNote(rel)
		if err != nil {
			return err
		}
		m := MetaOf(n)
		metas = append(metas, m)
		return batch.Index(rel, DocumentOf(m, string(n.Body)))
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := idx.idx.Batch(batch); err != nil {
		b.Fatal(err)
	}
	cat.Replace(metas, nil)
	return idx, cat
}

func percentile(d []time.Duration, p float64) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func BenchmarkSearch5000(b *testing.B) {
	v := generateVault(b, 5000)
	idx, cat := indexVault(b, v)
	ctx := context.Background()
	queries := []string{"паспорт", "budget meeting", `"project document"`, "zebra", "отчёт задач*"}
	var search, grep, quick []time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		t0 := time.Now()
		if _, err := idx.Search(ctx, Request{Query: q, Limit: 10}); err != nil {
			b.Fatal(err)
		}
		search = append(search, time.Since(t0))
		t0 = time.Now()
		if _, err := Grep(ctx, v, GrepRequest{Pattern: "zebra", Limit: 50}); err != nil {
			b.Fatal(err)
		}
		grep = append(grep, time.Since(t0))
		t0 = time.Now()
		cat.QuickOpen("note 42", 10)
		quick = append(quick, time.Since(t0))
	}
	b.ReportMetric(float64(percentile(search, 0.95).Microseconds())/1000, "search_p95_ms")
	b.ReportMetric(float64(percentile(grep, 0.95).Microseconds())/1000, "grep_p95_ms")
	b.ReportMetric(float64(percentile(quick, 0.95).Microseconds())/1000, "quickopen_p95_ms")
}
