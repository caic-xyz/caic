// Sentence fragmenting for local-stack assistant text-to-speech requests.

package voicertc

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const defaultSentenceFragmentMaxRunes = 180

// sentenceFragmenter turns assistant text into stable TTS requests.
//
// It preserves byte-for-byte input order: concatenating all fragments returned
// by push and flush reconstructs the original text. push emits complete
// sentences only after terminal punctuation, optional closing punctuation, and
// trailing whitespace make the sentence stable. It also emits long fragments at
// word boundaries so text without sentence punctuation cannot grow without
// bound. flush emits the final incomplete sentence at end of stream.
type sentenceFragmenter struct {
	buf      string
	maxRunes int
}

// newSentenceFragmenter returns a fragmenter with maxRunes as the soft maximum
// buffered rune count before a size-limit cut.
//
// Non-positive maxRunes uses defaultSentenceFragmentMaxRunes.
func newSentenceFragmenter(maxRunes int) *sentenceFragmenter {
	if maxRunes <= 0 {
		maxRunes = defaultSentenceFragmentMaxRunes
	}
	return &sentenceFragmenter{maxRunes: maxRunes}
}

// push appends text and returns fragments ready for TTS.
//
// It returns nil when the buffered text has no stable sentence cut and remains
// under the size limit. Returned fragments are removed from the buffer.
func (f *sentenceFragmenter) push(text string) []string {
	if text == "" {
		return nil
	}
	f.buf += text
	var out []string
	for f.buf != "" {
		cut := stableSentenceCut(f.buf)
		if cut == 0 {
			cut = sizeLimitCut(f.buf, f.maxRunes)
		}
		if cut == 0 {
			break
		}
		out = append(out, f.buf[:cut])
		f.buf = f.buf[cut:]
	}
	return out
}

// flush returns the buffered tail and resets the fragmenter.
func (f *sentenceFragmenter) flush() []string {
	if f.buf == "" {
		return nil
	}
	out := []string{f.buf}
	f.buf = ""
	return out
}

func stableSentenceCut(text string) int {
	for i, r := range text {
		if !isSentencePunctuation(r) {
			continue
		}
		end := i + utf8.RuneLen(r)
		for end < len(text) {
			next, size := utf8.DecodeRuneInString(text[end:])
			if isSentencePunctuation(next) || isSentenceCloser(next) {
				end += size
				continue
			}
			break
		}
		sawSpace := false
		for end < len(text) {
			next, size := utf8.DecodeRuneInString(text[end:])
			if !unicode.IsSpace(next) {
				break
			}
			sawSpace = true
			end += size
		}
		if sawSpace {
			return end
		}
	}
	return 0
}

func sizeLimitCut(text string, maxRunes int) int {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return 0
	}
	count := 0
	cut := 0
	lastSpace := 0
	for i, r := range text {
		if count == maxRunes {
			break
		}
		count++
		end := i + utf8.RuneLen(r)
		cut = end
		if unicode.IsSpace(r) {
			lastSpace = end
		}
	}
	if lastSpace > 0 && strings.TrimSpace(text[:lastSpace]) != "" {
		return lastSpace
	}
	return cut
}

func isSentencePunctuation(r rune) bool {
	switch r {
	case '.', '!', '?':
		return true
	default:
		return false
	}
}

func isSentenceCloser(r rune) bool {
	switch r {
	case '\'', '"', ')', ']', '}', '»', '”', '’':
		return true
	default:
		return false
	}
}
