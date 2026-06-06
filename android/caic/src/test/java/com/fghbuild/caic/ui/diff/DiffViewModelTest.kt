// Unit tests for the diff splitting and file path extraction logic.
package com.fghbuild.caic.ui.diff

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class DiffViewModelTest {

    // ---- splitDiff ----

    @Test
    fun `splitDiff returns empty for blank input`() {
        assertTrue(DiffViewModel.splitDiff("").isEmpty())
        assertTrue(DiffViewModel.splitDiff("   \n  ").isEmpty())
    }

    @Test
    fun `splitDiff handles single file`() {
        val raw = """
            |diff --git a/src/main.kt b/src/main.kt
            |index abc1234..def5678 100644
            |--- a/src/main.kt
            |+++ b/src/main.kt
            |@@ -1,3 +1,4 @@
            |+added line
            | unchanged
        """.trimMargin()
        val diffs = DiffViewModel.splitDiff(raw)
        assertEquals(1, diffs.size)
        assertEquals("src/main.kt", diffs[0].path)
        assertTrue(diffs[0].content.contains("added line"))
        assertTrue(diffs[0].lines.any { it.text == "+added line" && it.kind == DiffLineKind.Added })
    }

    @Test
    fun `splitDiff handles multiple files`() {
        val raw = """
            |diff --git a/file1.kt b/file1.kt
            |--- a/file1.kt
            |+++ b/file1.kt
            |@@ -1 +1,2 @@
            |+hello
            |diff --git a/file2.kt b/file2.kt
            |--- a/file2.kt
            |+++ b/file2.kt
            |@@ -1 +1,2 @@
            |+world
        """.trimMargin()
        val diffs = DiffViewModel.splitDiff(raw)
        assertEquals(2, diffs.size)
        assertEquals("file1.kt", diffs[0].path)
        assertEquals("file2.kt", diffs[1].path)
        assertTrue(diffs[0].content.contains("+hello"))
        assertTrue(diffs[1].content.contains("+world"))
    }

    @Test
    fun `splitDiff preserves diff header in content`() {
        val raw = "diff --git a/foo.kt b/foo.kt\n--- a/foo.kt\n+++ b/foo.kt\n@@ -1 +1 @@\n-old\n+new"
        val diffs = DiffViewModel.splitDiff(raw)
        assertEquals(1, diffs.size)
        assertTrue(diffs[0].content.startsWith("diff --git"))
    }

    @Test
    fun `splitDiff handles files with no trailing newline in content`() {
        val raw = "diff --git a/empty.kt b/empty.kt\nBinary files differ"
        val diffs = DiffViewModel.splitDiff(raw)
        assertEquals(1, diffs.size)
        assertEquals("empty.kt", diffs[0].path)
        assertTrue(diffs[0].content.contains("Binary files differ"))
    }

    @Test
    fun `splitDiff preserves moved annotations across files`() {
        val raw = """
            |diff --git a/old.kt b/old.kt
            |--- a/old.kt
            |+++ b/old.kt
            |@@ -1,3 +0,0 @@
            |-fun movedBetweenFiles() {
            |-    return movedBetweenFilesValue
            |-}
            |diff --git a/new.kt b/new.kt
            |--- a/new.kt
            |+++ b/new.kt
            |@@ -0,0 +1,3 @@
            |+fun movedBetweenFiles() {
            |+    return movedBetweenFilesValue
            |+}
        """.trimMargin()

        val diffs = DiffViewModel.splitDiff(raw)

        assertEquals(2, diffs.size)
        assertEquals(3, diffs[0].lines.count { it.kind == DiffLineKind.MovedDeleted })
        assertEquals(3, diffs[1].lines.count { it.kind == DiffLineKind.MovedAdded })
    }

    // ---- extractPath ----

    @Test
    fun `extractPath from plus-plus-plus line with a slash prefix`() {
        val section = """
            |--- a/src/main.kt
            |+++ b/src/main.kt
            |@@ -1 +1,2 @@
        """.trimMargin()
        assertEquals("src/main.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath from plus-plus-plus line without slash prefix`() {
        val section = """
            |--- a/main.kt
            |+++ main.kt
            |@@ -1 +1 @@
        """.trimMargin()
        assertEquals("main.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath ignores plus-plus-plus dev-null`() {
        // New file: +++ b/newfile.kt and --- /dev/null
        val section = """
            |diff --git a/newfile.kt b/newfile.kt
            |new file mode 100644
            |--- /dev/null
            |+++ b/newfile.kt
            |@@ -0,0 +1 @@
            |+hello
        """.trimMargin()
        assertEquals("newfile.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath falls back to minus-minus-minus for deleted files`() {
        // Deleted file: --- a/deleted.kt and +++ /dev/null
        val section = """
            |diff --git a/deleted.kt b/deleted.kt
            |deleted file mode 100644
            |--- a/deleted.kt
            |+++ /dev/null
            |@@ -1 +0,0 @@
            |-bye
        """.trimMargin()
        assertEquals("deleted.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath uses rename line when present`() {
        val section = """
            |diff --git a/old.kt b/new.kt
            |similarity index 100%
            |rename from old.kt
            |rename to new.kt
        """.trimMargin()
        assertEquals("new.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath falls back to diff header with equal a and b`() {
        // Binary file diff with no +++/--- lines.
        val section = """
            |diff --git a/binary.dat b/binary.dat
            |Binary files differ
        """.trimMargin()
        assertEquals("binary.dat", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath falls back to diff header without a-b slash prefix`() {
        val section = """
            |diff --git simple.txt simple.txt
            |index 0000000..1111111
        """.trimMargin()
        assertEquals("simple.txt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `extractPath returns unknown for unrecognizable input`() {
        assertEquals("unknown", DiffViewModel.extractPath("completely random text"))
    }

    @Test
    fun `extractPath returns unknown for empty section`() {
        assertEquals("unknown", DiffViewModel.extractPath(""))
    }

    @Test
    fun `extractPath uses plus-plus-plus over rename when both present`() {
        // When both +++ and rename lines exist, +++ takes priority.
        val section = """
            |diff --git a/old.kt b/new.kt
            |rename to new.kt
            |--- a/old.kt
            |+++ b/old.kt
            |@@ -1 +1 @@
        """.trimMargin()
        assertEquals("old.kt", DiffViewModel.extractPath(section))
    }

    @Test
    fun `annotateDiffLines marks matching added and deleted blocks as moved`() {
        val diff = """
            |diff --git a/src/main.kt b/src/main.kt
            |--- a/src/main.kt
            |+++ b/src/main.kt
            |@@ -1,8 +1,8 @@
            | val keep = true
            |-fun movedExample() {
            |-    return alphaBetaGammaDelta
            |-}
            | val middle = true
            |+fun movedExample() {
            |+    return alphaBetaGammaDelta
            |+}
        """.trimMargin()

        val lines = DiffLineClassifier.annotateDiffLines(diff)

        assertEquals(3, lines.count { it.kind == DiffLineKind.MovedDeleted })
        assertEquals(3, lines.count { it.kind == DiffLineKind.MovedAdded })
    }

    @Test
    fun `annotateDiffLines does not mark matching blocks below alphanumeric threshold`() {
        val diff = """
            |diff --git a/src/main.kt b/src/main.kt
            |--- a/src/main.kt
            |+++ b/src/main.kt
            |@@ -1,5 +1,5 @@
            |-x = 1
            | context
            |+x = 1
        """.trimMargin()

        val lines = DiffLineClassifier.annotateDiffLines(diff)

        assertTrue(lines.none { it.kind == DiffLineKind.MovedDeleted || it.kind == DiffLineKind.MovedAdded })
    }

    @Test
    fun `annotateDiffLines alternates moved block variants`() {
        val diff = """
            |diff --git a/src/main.kt b/src/main.kt
            |--- a/src/main.kt
            |+++ b/src/main.kt
            |@@ -1,12 +1,12 @@
            |-fun firstMovedBlock() {
            |-    return firstMovedValue
            |-}
            | context
            |-fun secondMovedBlock() {
            |-    return secondMovedValue
            |-}
            | context
            |+fun firstMovedBlock() {
            |+    return firstMovedValue
            |+}
            | context
            |+fun secondMovedBlock() {
            |+    return secondMovedValue
            |+}
        """.trimMargin()

        val movedDeleted = DiffLineClassifier.annotateDiffLines(diff)
            .filter { it.kind == DiffLineKind.MovedDeleted }

        assertEquals(0, movedDeleted[0].movedVariant)
        assertEquals(1, movedDeleted[3].movedVariant)
    }
}
