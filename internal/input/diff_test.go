package input

import (
	"slices"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

func TestParseUnifiedDiffAndValidLines(t *testing.T) {
	diff := `diff --git a/internal/auth/service.go b/internal/auth/service.go
index 1111111..2222222 100644
--- a/internal/auth/service.go
+++ b/internal/auth/service.go
@@ -10,4 +10,5 @@ func Verify(tok string) error {
 	if tok == "" {
-		return nil
+		return ErrEmpty
+		// fixed
 	}
 	return nil
diff --git a/internal/newfile.go b/internal/newfile.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/internal/newfile.go
@@ -0,0 +1,3 @@
+package internal
+
+func Hello() {}
diff --git a/internal/old.go b/internal/old.go
deleted file mode 100644
index 4444444..0000000
--- a/internal/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package internal
-func Old() {}
diff --git a/old_name.go b/new_name.go
similarity index 90%
rename from old_name.go
rename to new_name.go
index 5555555..6666666 100644
--- a/old_name.go
+++ b/new_name.go
@@ -1,2 +1,2 @@
 package main
-var x = 1
+var x = 2
`

	files := ParseUnifiedDiff(diff)
	if len(files) != 4 {
		t.Fatalf("len(files) = %d, want 4", len(files))
	}

	// 1. Modified file
	f0 := files[0]
	if f0.Path != "internal/auth/service.go" || f0.Status != "modified" {
		t.Fatalf("f0 = %+v, want modified internal/auth/service.go", f0)
	}
	if f0.Additions != 2 || f0.Deletions != 1 {
		t.Fatalf("f0 counts = +%d/-%d, want +2/-1", f0.Additions, f0.Deletions)
	}
	wantHunks0 := []domain.LineRange{{StartLine: 10, EndLine: 14}}
	if !slices.Equal(f0.ChangedHunks, wantHunks0) {
		t.Fatalf("f0.ChangedHunks = %+v, want %+v", f0.ChangedHunks, wantHunks0)
	}
	valid0 := ValidDiffLines(f0.Patch)
	for _, ln := range []int{10, 11, 12, 13, 14} {
		if !valid0[ln] {
			t.Errorf("expected line %d to be in ValidDiffLines", ln)
		}
	}
	if valid0[9] || valid0[15] {
		t.Errorf("unexpected lines outside hunk in ValidDiffLines: %v", valid0)
	}

	// 2. Added file
	f1 := files[1]
	if f1.Path != "internal/newfile.go" || f1.Status != "added" || f1.Additions != 3 {
		t.Fatalf("f1 = %+v, want added internal/newfile.go (+3)", f1)
	}

	// 3. Deleted file
	f2 := files[2]
	if f2.Path != "internal/old.go" || f2.Status != "deleted" || len(f2.ChangedHunks) != 0 {
		t.Fatalf("f2 = %+v, want deleted internal/old.go with no hunks", f2)
	}

	// 4. Renamed file
	f3 := files[3]
	if f3.Path != "new_name.go" || f3.PreviousPath != "old_name.go" || f3.Status != "renamed" {
		t.Fatalf("f3 = %+v, want renamed old_name.go -> new_name.go", f3)
	}

	byFile := ParseDiffLinesByFile(diff)
	if len(byFile) != 3 {
		t.Fatalf("len(ParseDiffLinesByFile) = %d, want 3 (deleted omitted)", len(byFile))
	}
	if !byFile["new_name.go"][2] {
		t.Fatalf("expected line 2 in new_name.go diff lines")
	}
	if byFile["new_name.go"][3] {
		t.Fatalf("unexpected phantom line 3 past end of hunk in new_name.go: %v", byFile["new_name.go"])
	}
}

func TestValidDiffLines_TrailingNewlineAndBlankContext(t *testing.T) {
	d := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n x\n+y\n z\n"
	byFile := ParseDiffLinesByFile(d)
	got := byFile["a.go"]
	if len(got) != 3 || !got[1] || !got[2] || !got[3] || got[4] {
		t.Fatalf("ParseDiffLinesByFile = %v, want lines {1,2,3} only", got)
	}

	// Blank context line inside hunk with stripped space should still count as line 2.
	patchWithBlank := "@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n"
	gotBlank := ValidDiffLines(patchWithBlank)
	if len(gotBlank) != 3 || !gotBlank[1] || !gotBlank[2] || !gotBlank[3] || gotBlank[4] {
		t.Fatalf("ValidDiffLines(blank context) = %v, want {1,2,3}", gotBlank)
	}
}

func TestParseUnifiedDiff_CountsPlusPlusAndMinusMinusContentLines(t *testing.T) {
	diff := `diff --git a/q.sql b/q.sql
index 1111111..2222222 100644
--- a/q.sql
+++ b/q.sql
@@ -1,3 +1,1 @@
 SELECT 1;
--- old comment 1
--- old comment 2
diff --git a/notes.md b/notes.md
index 3333333..4444444 100644
--- a/notes.md
+++ b/notes.md
@@ -1,1 +1,2 @@
 header
+++ added line
`

	files := ParseUnifiedDiff(diff)
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Path != "q.sql" || files[0].Additions != 0 || files[0].Deletions != 2 {
		t.Fatalf("q.sql counts = +%d/-%d, want +0/-2", files[0].Additions, files[0].Deletions)
	}
	if files[1].Path != "notes.md" || files[1].Additions != 1 || files[1].Deletions != 0 {
		t.Fatalf("notes.md counts = +%d/-%d, want +1/-0", files[1].Additions, files[1].Deletions)
	}
}

