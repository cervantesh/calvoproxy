package compress

// Dialects.
//
// The same conversation arrives in two shapes. In the chat-completions dialect
// a tool result is a message with Role "tool" and a plain string Content. In
// the Anthropic /v1/messages dialect it is a message with Role "user" whose
// Content is an array of blocks, and the tool output lives inside a block of
// type "tool_result" — whose own content is either a string or a further array
// of {"type":"text","text":…} blocks.
//
// An engine should not have to know that. A segment is one addressable piece of
// tool-result text somewhere inside a message, plus enough information to write
// a replacement back into exactly that position. Engines iterate segments and
// stay dialect-blind.
//
// The rules that keep this safe are the package's rules, applied to shapes:
//
//   - a block whose shape is not recognised is SKIPPED, never coerced. There is
//     no attempt to guess where the text is in a block type we do not know;
//   - writing back copies the arrays and maps along the path, so the caller's
//     structures are never mutated and every sibling field of the block
//     (tool_use_id, type, is_error, cache_control…) survives untouched;
//   - a plain string on a "user" message is never a tool result. It is what the
//     human said, and compressing it would be compressing the question.
//
// One deliberate asymmetry: tool_result blocks are recognised on Role "user"
// only. A "tool"-role message carrying a tool_result block is a shape no real
// dialect produces, and inventing a meaning for a hybrid is exactly the kind of
// guess this package refuses to make.

// MaxSegmentBytes is the largest single tool result any engine will process.
//
// The engines that rewrite text split it into lines and rebuild it, so peak
// memory is a small multiple of the segment; a pathological 500 MB tool result
// would exhaust the process before it compressed anything. 8 MiB is far above
// any tool output a model could usefully read — a 200k-token context is roughly
// 800 KB of text, so this is an order of magnitude past the point where
// compressing is even the right response — and it bounds the worst case at tens
// of megabytes. Above it, engines return the input unchanged: degrading to
// "did nothing" is always a valid answer, and an OOM never is.
const MaxSegmentBytes = 8 << 20

// segKind says where a segment lives, which is what tells replace how to put it
// back.
type segKind uint8

const (
	// segWholeContent: Content is the string itself.
	segWholeContent segKind = iota
	// segTextBlock: Content[block] is {"type":"text","text":…}.
	segTextBlock
	// segToolResultString: Content[block] is a tool_result whose content is a string.
	segToolResultString
	// segToolResultText: Content[block].content[inner] is {"type":"text","text":…}.
	segToolResultText
)

// segment is one piece of tool-result text and its address inside a message.
type segment struct {
	Text string
	// index is the segment's ordinal within its message. Engines that compare
	// segments across messages (Dedup) need a stable identity for one.
	index int

	kind  segKind
	block int
	inner int
}

// The scanners append into a caller-supplied slice so an engine can reuse one
// buffer across a whole history. A per-message allocation is most of the cost
// on the shape that matters for it: a long conversation of small messages.
//
// unlimited is the byte ceiling for callers that only want to measure. Engines
// pass MaxSegmentBytes.
const unlimited = int(^uint(0) >> 1)

// segScan collects segments, dropping any that are over the ceiling.
type segScan struct {
	dst   []segment
	limit int
	// count is the ordinal among every recognised segment, including the ones
	// the ceiling dropped, so a segment's identity does not depend on which
	// caller scanned it.
	count int
}

func (s *segScan) add(text string, kind segKind, block, inner int) {
	index := s.count
	s.count++
	if len(text) > s.limit {
		return
	}
	s.dst = append(s.dst, segment{Text: text, index: index, kind: kind, block: block, inner: inner})
}

// appendContentSegments appends every piece of text in a message that an engine
// may rewrite: plain string content whatever the role, plus tool-result text in
// the structured dialects.
//
// The plain-string case ignores the role on purpose. Dedup has always been able
// to collapse a repeated assistant or user block, and Size has always counted
// them; narrowing that to tool results would be a silent behaviour change.
func appendContentSegments(dst []segment, m Message, limit int) []segment {
	if text, ok := m.Text(); ok {
		scan := segScan{dst: dst, limit: limit}
		scan.add(text, segWholeContent, 0, 0)
		return scan.dst
	}
	return appendToolResultSegments(dst, m, limit)
}

// appendToolResultSegments appends the text that came back from a TOOL, in
// either dialect. Engines that exist to clean up command output use this one: a
// user's question is not theirs to rewrite, however long it is.
func appendToolResultSegments(dst []segment, m Message, limit int) []segment {
	scan := segScan{dst: dst, limit: limit}
	switch m.Role {
	case "tool":
		if text, ok := m.Text(); ok {
			scan.add(text, segWholeContent, 0, 0)
			break
		}
		// {"type":"text","text":…} blocks on a tool message.
		for i, raw := range blocksOf(m) {
			block, kind, ok := blockOf(raw)
			if !ok || kind != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok {
				scan.add(text, segTextBlock, i, 0)
			}
		}
	case "user":
		// The Anthropic shape: tool_result blocks, whose content is a string or
		// an array of text blocks.
		for i, raw := range blocksOf(m) {
			block, kind, ok := blockOf(raw)
			if !ok || kind != "tool_result" {
				continue
			}
			switch content := block["content"].(type) {
			case string:
				scan.add(content, segToolResultString, i, 0)
			case []any:
				for j, rawInner := range content {
					inner, innerKind, ok := blockOf(rawInner)
					if !ok || innerKind != "text" {
						continue
					}
					if text, ok := inner["text"].(string); ok {
						scan.add(text, segToolResultText, i, j)
					}
				}
			}
		}
	}
	return scan.dst
}

// toolResultSegments and contentSegments are the allocating forms, for callers
// that measure rather than rewrite and have no buffer to reuse.
func toolResultSegments(m Message) []segment {
	return appendToolResultSegments(nil, m, unlimited)
}

func contentSegments(m Message) []segment {
	return appendContentSegments(nil, m, unlimited)
}

func blocksOf(m Message) []any {
	blocks, _ := m.Content.([]any)
	return blocks
}

// replace returns a message with this segment's text swapped for text.
//
// Everything else is preserved: the other blocks, the other fields of this
// block, and Extra. The structures along the path are copied rather than
// edited, because the caller still owns the originals — an engine that mutated
// them would corrupt a request that may already have been sent.
//
// If the message no longer has the shape the segment was scanned from, the
// message comes back untouched. That cannot happen through this package's own
// engines; it is the do-nothing answer to a caller that surprises us.
func (s segment) replace(m Message, text string) Message {
	if s.kind == segWholeContent {
		m.Content = text
		return m
	}
	blocks, ok := m.Content.([]any)
	if !ok || s.block < 0 || s.block >= len(blocks) {
		return m
	}
	block, ok := blocks[s.block].(map[string]any)
	if !ok {
		return m
	}

	newBlock := copyBlock(block)
	switch s.kind {
	case segTextBlock:
		newBlock["text"] = text
	case segToolResultString:
		newBlock["content"] = text
	case segToolResultText:
		inner, ok := block["content"].([]any)
		if !ok || s.inner < 0 || s.inner >= len(inner) {
			return m
		}
		innerBlock, ok := inner[s.inner].(map[string]any)
		if !ok {
			return m
		}
		newInner := make([]any, len(inner))
		copy(newInner, inner)
		replacement := copyBlock(innerBlock)
		replacement["text"] = text
		newInner[s.inner] = replacement
		newBlock["content"] = newInner
	default:
		return m
	}

	newBlocks := make([]any, len(blocks))
	copy(newBlocks, blocks)
	newBlocks[s.block] = newBlock
	m.Content = newBlocks
	return m
}

// blockOf reads a content block and its declared type. A block that is not an
// object, or whose type is not a string, is not one we understand.
func blockOf(raw any) (map[string]any, string, bool) {
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, "", false
	}
	kind, ok := block["type"].(string)
	if !ok {
		return nil, "", false
	}
	return block, kind, true
}

func copyBlock(block map[string]any) map[string]any {
	out := make(map[string]any, len(block))
	for k, v := range block {
		out[k] = v
	}
	return out
}
