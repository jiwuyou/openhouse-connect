package core

import (
	"path/filepath"
	"testing"
)

func TestExtractArtifactReferences(t *testing.T) {
	text := "排版已经优化。\nMEDIA : `/tmp/generated image.png`\nFILE:/tmp/report.pdf\n"

	cleaned, refs := extractArtifactReferences(text)
	if cleaned != "排版已经优化。" {
		t.Fatalf("cleaned = %q, want visible text only", cleaned)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %#v, want 2", refs)
	}
	if refs[0].Kind != artifactTagMedia || refs[0].Path != "/tmp/generated image.png" {
		t.Fatalf("media ref = %#v", refs[0])
	}
	if refs[1].Kind != artifactTagFile || refs[1].Path != "/tmp/report.pdf" {
		t.Fatalf("file ref = %#v", refs[1])
	}
}

func TestExtractArtifactReferences_IgnoresRelativePaths(t *testing.T) {
	cleaned, refs := extractArtifactReferences("MEDIA:./out/generated.png")
	if cleaned != "MEDIA:./out/generated.png" {
		t.Fatalf("cleaned = %q, want original text", cleaned)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want none", refs)
	}
}

func TestNormalizeArtifactTagPath_FileURL(t *testing.T) {
	got := normalizeArtifactTagPath("file:///tmp/generated%20image.png")
	if got != filepath.Clean("/tmp/generated image.png") {
		t.Fatalf("path = %q", got)
	}
}
