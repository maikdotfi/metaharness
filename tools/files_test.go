package tools

// This file holds fixtures shared by the read_file, write_file, and edit_file
// tests. The reusable machinery they lean on — a real-shell Sandbox, the temp
// ExecCtx, and the seed/onDisk/CallTool helpers — lives in the testutils
// package, so these tests exercise the actual cat / base64 plumbing, not a mock.

// sampleText is the shared fixture content. It deliberately packs in characters
// that are awkward to move through a shell — single and double quotes, a dollar
// sign, a backslash, a non-ASCII line, and leading/trailing whitespace — so the
// base64 round-trip in putFile is genuinely stressed rather than getting lucky
// with plain ASCII.
const sampleText = "line one\n" +
	"two's a crowd: \"quoted\", $VAR, back\\slash\n" +
	"café — unicode ☕\n" +
	"  indented tail  \n"
