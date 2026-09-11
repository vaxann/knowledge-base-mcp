# Benchmarks

Generated vault: 5,000 notes in 25 folders, 5 sections of 80 mixed Russian/English words each, on-disk Bleve index. Run with:

```bash
go test -run '^$' -bench BenchmarkSearch5000 -benchtime 200x ./internal/search/
```

Results on a 12-core Linux workstation (2026-09-11, Go 1.26, Bleve 2.6.1):

| Operation | p95 | Target |
|---|---|---|
| `kb_search` (ranked, stemmed, highlighted) | 22 ms | ≤ 100 ms |
| `kb_grep` (literal, parallel scan) | 99 ms | ≤ 200 ms |
| `kb_quick_open` (fuzzy titles/paths) | 11 ms | — |
