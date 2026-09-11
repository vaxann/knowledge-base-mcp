package kb

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

func TestLogsNeverContainNoteBodies(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig(t, testutil.NewVaultRepo(t))
	s, err := Open(context.Background(), cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const marker = "MARKER_SECRET_BODY_9f3a"
	ctx := WithClient(context.Background(), "log-test")
	if _, err := s.ReplaceNote(ctx, "Projects/Alpha.md", "---\ntitle: "+marker+"\n---\n# X\n\n"+marker+"\n", "", "summary "+marker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNote(ctx, "Notes/Log.md", marker, map[string]any{"secret": marker}, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "Projects/Alpha.md"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), marker) {
		t.Errorf("log output leaks note content:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Projects/Alpha.md") {
		t.Error("expected at least a debug line naming the path")
	}
}
