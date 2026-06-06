// Diff line classifier that detects zebra-style moved blocks in unified diffs.
package com.fghbuild.caic.ui.diff

enum class DiffLineKind {
    Context,
    Added,
    Deleted,
    Hunk,
    Header,
    MovedAdded,
    MovedDeleted,
}

data class DiffLine(
    val text: String,
    val kind: DiffLineKind,
    val movedVariant: Int? = null,
)

private data class ChangeLine(
    val lineIndex: Int,
    val content: String,
)

private data class MovedBlock(
    val addedLineIndexes: List<Int>,
    val deletedLineIndexes: List<Int>,
)

object DiffLineClassifier {
    private const val MIN_MOVED_BLOCK_ALNUM_CHARS = 20

    fun annotateDiffLines(diff: String): List<DiffLine> {
        val rawLines = diff.split("\n")
        val lines = rawLines.map { line -> DiffLine(line, lineKind(line)) }.toMutableList()
        val addedByLineIndex = mutableMapOf<Int, ChangeLine>()
        val deletedByLineIndex = mutableMapOf<Int, ChangeLine>()
        val addedLineIndexesByContent = mutableMapOf<String, MutableList<Int>>()

        rawLines.forEachIndexed { index, line ->
            when {
                isAddedLine(line) -> {
                    val change = ChangeLine(index, line.drop(1))
                    addedByLineIndex[index] = change
                    addedLineIndexesByContent.getOrPut(change.content) { mutableListOf() }.add(index)
                }
                isDeletedLine(line) -> deletedByLineIndex[index] = ChangeLine(index, line.drop(1))
            }
        }

        findMovedBlocks(
            deletedByLineIndex = deletedByLineIndex,
            addedByLineIndex = addedByLineIndex,
            addedLineIndexesByContent = addedLineIndexesByContent,
        ).forEachIndexed { blockIndex, block ->
            val movedVariant = blockIndex % 2
            block.addedLineIndexes.forEach { lineIndex ->
                lines[lineIndex] = DiffLine(lines[lineIndex].text, DiffLineKind.MovedAdded, movedVariant)
            }
            block.deletedLineIndexes.forEach { lineIndex ->
                lines[lineIndex] = DiffLine(lines[lineIndex].text, DiffLineKind.MovedDeleted, movedVariant)
            }
        }

        return lines
    }

    private fun lineKind(line: String): DiffLineKind = when {
        line.startsWith("@@") -> DiffLineKind.Hunk
        line.startsWith("diff ") ||
            line.startsWith("index ") ||
            line.startsWith("---") ||
            line.startsWith("+++") -> DiffLineKind.Header
        line.startsWith("+") -> DiffLineKind.Added
        line.startsWith("-") -> DiffLineKind.Deleted
        else -> DiffLineKind.Context
    }

    private fun isAddedLine(line: String): Boolean = line.startsWith("+") && !line.startsWith("+++")

    private fun isDeletedLine(line: String): Boolean = line.startsWith("-") && !line.startsWith("---")

    private fun findMovedBlocks(
        deletedByLineIndex: Map<Int, ChangeLine>,
        addedByLineIndex: Map<Int, ChangeLine>,
        addedLineIndexesByContent: Map<String, List<Int>>,
    ): List<MovedBlock> {
        val blocks = mutableListOf<MovedBlock>()
        val usedAddedLineIndexes = mutableSetOf<Int>()
        val usedDeletedLineIndexes = mutableSetOf<Int>()

        for (deletedStart in deletedByLineIndex.keys.sorted()) {
            val block = findBestMovedBlock(
                deletedStart = deletedStart,
                deletedByLineIndex = deletedByLineIndex,
                addedByLineIndex = addedByLineIndex,
                addedLineIndexesByContent = addedLineIndexesByContent,
                usedDeletedLineIndexes = usedDeletedLineIndexes,
                usedAddedLineIndexes = usedAddedLineIndexes,
            )
            block?.let {
                usedAddedLineIndexes.addAll(block.addedLineIndexes)
                usedDeletedLineIndexes.addAll(block.deletedLineIndexes)
                blocks.add(block)
            }
        }

        return blocks
    }

    private fun findBestMovedBlock(
        deletedStart: Int,
        deletedByLineIndex: Map<Int, ChangeLine>,
        addedByLineIndex: Map<Int, ChangeLine>,
        addedLineIndexesByContent: Map<String, List<Int>>,
        usedDeletedLineIndexes: Set<Int>,
        usedAddedLineIndexes: Set<Int>,
    ): MovedBlock? {
        val deleted = deletedByLineIndex[deletedStart]
        if (deleted == null || deletedStart in usedDeletedLineIndexes) return null

        return addedLineIndexesByContent[deleted.content]
            .orEmpty()
            .filterNot { addedStart -> addedStart in usedAddedLineIndexes }
            .map { addedStart ->
                expandMovedBlock(
                    deletedStart = deletedStart,
                    addedStart = addedStart,
                    deletedByLineIndex = deletedByLineIndex,
                    addedByLineIndex = addedByLineIndex,
                    usedDeletedLineIndexes = usedDeletedLineIndexes,
                    usedAddedLineIndexes = usedAddedLineIndexes,
                )
            }
            .map { block -> block to countMovedBlockAlnum(block, deletedByLineIndex) }
            .filter { (_, alnumCount) -> alnumCount >= MIN_MOVED_BLOCK_ALNUM_CHARS }
            .maxByOrNull { (_, alnumCount) -> alnumCount }
            ?.first
    }

    private fun expandMovedBlock(
        deletedStart: Int,
        addedStart: Int,
        deletedByLineIndex: Map<Int, ChangeLine>,
        addedByLineIndex: Map<Int, ChangeLine>,
        usedDeletedLineIndexes: Set<Int>,
        usedAddedLineIndexes: Set<Int>,
    ): MovedBlock {
        val addedLineIndexes = mutableListOf<Int>()
        val deletedLineIndexes = mutableListOf<Int>()
        var deletedIndex = deletedStart
        var addedIndex = addedStart

        while (deletedIndex !in usedDeletedLineIndexes && addedIndex !in usedAddedLineIndexes) {
            val deleted = deletedByLineIndex[deletedIndex]
            val added = addedByLineIndex[addedIndex]
            if (deleted == null || added == null || deleted.content != added.content) break
            deletedLineIndexes.add(deletedIndex)
            addedLineIndexes.add(addedIndex)
            deletedIndex++
            addedIndex++
        }

        return MovedBlock(
            addedLineIndexes = addedLineIndexes,
            deletedLineIndexes = deletedLineIndexes,
        )
    }

    private fun countMovedBlockAlnum(
        block: MovedBlock,
        deletedByLineIndex: Map<Int, ChangeLine>,
    ): Int = block.deletedLineIndexes.sumOf { lineIndex ->
        deletedByLineIndex[lineIndex]?.content.orEmpty().count { it.isLetterOrDigit() }
    }
}
