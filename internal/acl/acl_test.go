package acl

import "testing"

func TestLevel(t *testing.T) {
	cases := []struct {
		name  string
		rules Set
		path  string
		want  Level
	}{
		{"no rules", nil, "Projects/a.md", None},
		{"root read", Set{{"", Read}}, "Projects/a.md", Read},
		{"root rule applies to root", Set{{"", Write}}, "", Write},
		{"closed subfolder inside open folder", Set{{"Projects", Write}, {"Projects/Secret", None}}, "Projects/Secret/b.md", None},
		{"open folder around closed subfolder", Set{{"Projects", Write}, {"Projects/Secret", None}}, "Projects/a.md", Write},
		{"open subfolder inside closed repository", Set{{"Shared/Team", Read}}, "Shared/Team/x.md", Read},
		{"sibling of an open subfolder", Set{{"Shared/Team", Read}}, "Shared/y.md", None},
		{"segment boundary", Set{{"Work", Read}}, "Workshop/a.md", None},
		{"folder itself", Set{{"Work", Read}}, "Work", Read},
		{"deeper raises", Set{{"", None}, {"A", Read}, {"A/B", Write}}, "A/B/c.md", Write},
		{"deeper lowers", Set{{"", Write}, {"A", Read}}, "A/x.md", Read},
		{"order does not matter", Set{{"A/B", Write}, {"A", Read}, {"", None}}, "A/B/c.md", Write},
		{"three levels reopen", Set{{"A", Read}, {"A/B", None}, {"A/B/C", Write}}, "A/B/C/d.md", Write},
		{"three levels closed middle", Set{{"A", Read}, {"A/B", None}, {"A/B/C", Write}}, "A/B/x.md", None},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rules.Level(c.path); got != c.want {
				t.Errorf("Level(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestTraversable(t *testing.T) {
	deep := Set{{"Shared/Team", Read}}
	cases := []struct {
		name   string
		rules  Set
		folder string
		want   bool
	}{
		{"no rules root", nil, "", false},
		{"root leads to a deep grant", deep, "", true},
		{"parent leads to a deep grant", deep, "Shared", true},
		{"granted folder", deep, "Shared/Team", true},
		{"below granted folder", deep, "Shared/Team/Sub", true},
		{"sibling folder", deep, "Shared/Other", false},
		{"unrelated folder", deep, "Private", false},
		{"similar prefix", deep, "Share", false},
		{"hidden subfolder", Set{{"Projects", Read}, {"Projects/Secret", None}}, "Projects/Secret", false},
		{"reopened below a hidden folder", Set{{"A", Read}, {"A/B", None}, {"A/B/C", Read}}, "A/B", true},
		{"only none rules", Set{{"", None}, {"A", None}}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rules.Traversable(c.folder); got != c.want {
				t.Errorf("Traversable(%q) = %v, want %v", c.folder, got, c.want)
			}
		})
	}
}

func TestMatchAndHelpers(t *testing.T) {
	s := Set{{"Projects", Write}, {"Projects/Docs", Read}}
	if r, ok := s.Match("Projects/Docs/a.md"); !ok || r.Path != "Projects/Docs" {
		t.Errorf("Match = %+v, %v", r, ok)
	}
	if _, ok := s.Match("Other/a.md"); ok {
		t.Error("no rule must match Other/a.md")
	}
	if !s.CanWrite("Projects/a.md") || s.CanWrite("Projects/Docs/a.md") || !s.CanRead("Projects/Docs/a.md") {
		t.Error("CanRead/CanWrite disagree with the levels")
	}
	if !s.Any() || (Set{{"", None}}).Any() || Set(nil).Any() {
		t.Error("Any is wrong")
	}
	for _, l := range []Level{None, Read, Write} {
		if !l.Valid() {
			t.Errorf("%q must be valid", l)
		}
	}
	if Level("admin").Valid() || Level("").Valid() {
		t.Error("unknown levels must be invalid")
	}
}
