package redact

import (
	"strings"
	"testing"
)

// canonicalSecrets are whole, well-formed credentials of every shape the
// patterns in this package recognise, each paired with the separator variants
// its rule accepts. The truncation tests below slide a cut point through each
// one byte by byte.
var canonicalSecrets = []struct {
	name string
	text string
}{
	{"aws secret space", "AWS_SECRET_ACCESS_KEY: " + strings.Repeat("A", 40)},
	{"aws secret newline", "AWS_SECRET_ACCESS_KEY:\n" + strings.Repeat("A", 40)},
	{"aws secret tab", "AWS_SECRET_ACCESS_KEY:\t" + strings.Repeat("A", 40)},
	{"aws secret bare", "AWS_SECRET_ACCESS_KEY:" + strings.Repeat("A", 40)},
	{"aws secret spaced equals", "AWS_SECRET_ACCESS_KEY = " + strings.Repeat("A", 40)},
	{"aws access key id", "AKIA" + strings.Repeat("B", 16)},
	{"github classic", "ghp_" + strings.Repeat("k", 36)},
	{"github fine grained", "github_pat_" + strings.Repeat("z", 25)},
	{"google api key", "AIza" + strings.Repeat("c", 35)},
	{"jwt", "ey" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)},
	{"bearer", "Bearer " + strings.Repeat("t", 30)},
	// Ends on non-word characters, so the full rule's trailing \b does not
	// hold at the cut. The partial matcher has to catch it anyway.
	{"bearer punctuated", "Bearer " + strings.Repeat("/", 8) + strings.Repeat("A", 10)},
	{"postgres url", "postgres://u:" + strings.Repeat("p", 30) + "@h"},
	{"postgresql url", "postgresql://u:" + strings.Repeat("p", 30) + "@h"},
	{"stripe live", "sk_live_" + strings.Repeat("9", 20)},
	{"openai key", "sk-" + strings.Repeat("q", 24)},
	{"slack bot", "xoxb-" + strings.Repeat("1", 20)},
	{"slack app", "xapp-" + strings.Repeat("1", 20)},
	{"gitlab pat", "glpat-" + strings.Repeat("g", 22)},
	{"generic password", "PASSWORD=" + strings.Repeat("q", 30)},
	{"generic token newline", "TOKEN:\n" + strings.Repeat("q", 30)},
	{"pem rsa", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIE\n", 30) + "-----END RSA PRIVATE KEY-----"},
	{"pem ec", "-----BEGIN EC PRIVATE KEY-----\n" + strings.Repeat("MHcC\n", 20) + "-----END EC PRIVATE KEY-----"},
}

// TestPreviewPrefixNeverLeaksAcrossCut slides a cut point through every
// canonical secret and asserts the surviving prefix reveals no more than
// redacting the whole input would have.
//
// Two properties of this test are what make it meaningful, and both were
// learned by getting them wrong:
//
//   - It asserts a RECOGNISABLE PREFIX is absent, not the complete secret. Any
//     truncation whatsoever removes the complete secret, so asserting on that
//     passes trivially while real head-of-credential leaks go unnoticed.
//   - It compares against redacting the full input rather than against a fixed
//     expectation. Some inputs are not fully protected by the patterns even
//     without truncation; the contract here is that truncation adds no new
//     exposure, so the full-redaction result is the only correct yardstick.
//
// Each secret is exercised under several leading contexts. Whitespace before
// the secret is the obvious case, but several full rules — AWS secret,
// connection string, generic credential — carry no left \b and therefore also
// match a keyword glued to preceding text ("MY_AWS_SECRET_ACCESS_KEY=").
// An earlier version of this test used only the whitespace prefix, so it never
// exercised those occurrences: the partial matchers were stricter than their
// full rules and leaked, while every assertion here passed.
func TestPreviewPrefixNeverLeaksAcrossCut(t *testing.T) {
	leadings := []struct {
		name   string
		prefix string
	}{
		{"space separated", "prelude text "},
		{"glued to a word char", "prelude_text_MY_"},
		{"glued to a digit", "prelude 42"},
	}

	checked := 0
	for _, sec := range canonicalSecrets {
		for _, lead := range leadings {
			for cut := 3; cut <= len(sec.text); cut++ {
				visible := sec.text[:cut]

				full := lead.prefix + sec.text + " trailing text"
				_, got := PreviewPrefix(lead.prefix + visible)
				baseline := Text(full)

				checked++
				if strings.Contains(got, visible) && !strings.Contains(baseline, visible) {
					t.Errorf("%s (%s): cut at %d leaks %q, which full redaction removes",
						sec.name, lead.name, cut, visible[:min(40, len(visible))])
				}
			}
		}
	}
	if checked < 1500 {
		t.Fatalf("only %d cut points exercised; the corpus shrank unexpectedly", checked)
	}
	t.Logf("checked %d cut points", checked)
}

// Named regressions for shapes a whitespace-only corpus cannot reach. These are
// the counterexamples that exposed the partial/full mismatch; kept as their own
// test so a failure names the rule directly instead of surfacing as one row of
// a multi-thousand-case sweep.
func TestPreviewPrefixHandlesRulesWithoutLeftBoundary(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		cutLen int
	}{
		{
			// The full AWS rule has no \b, so it matches the keyword embedded in
			// a longer identifier. The partial matcher has to match there too.
			name:   "aws secret embedded in a longer identifier",
			text:   "MY_AWS_SECRET_ACCESS_KEY=" + strings.Repeat("A", 40),
			cutLen: len("MY_AWS_SECRET_ACCESS_KEY=") + 20,
		},
		{
			name:   "generic credential embedded in a longer identifier",
			text:   "APP_PASSWORD=" + strings.Repeat("z", 60),
			cutLen: len("APP_PASSWORD=") + 30,
		},
		{
			// The scheme group is (?:...)(?:ql)?:// so these spellings are
			// matched by the full rule and need partial coverage.
			name:   "mysqlql scheme",
			text:   "mysqlql://user:" + strings.Repeat("p", 60) + "@host",
			cutLen: len("mysqlql://user:") + 30,
		},
		{
			name:   "redisql scheme",
			text:   "redisql://user:" + strings.Repeat("p", 60) + "@host",
			cutLen: len("redisql://user:") + 30,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			visible := tc.text[:tc.cutLen]
			baseline := Text("prelude " + tc.text + " trailing")
			_, got := PreviewPrefix("prelude " + visible)

			if strings.Contains(baseline, visible) {
				t.Fatalf("test is vacuous: full redaction also leaks %q", visible[:min(40, len(visible))])
			}
			if strings.Contains(got, visible) {
				t.Errorf("preview leaks %q, which full redaction removes", visible[:min(40, len(visible))])
			}
		})
	}
}

// TestEverySecretPatternHasPartial is the contract that keeps this scheme
// honest as patterns are added. A rule without a partial matcher silently loses
// truncation coverage: it still redacts complete secrets, so every other test
// passes, and only a straddling credential in production reveals the gap.
//
// The PEM rule is exempt because RE2 has no lookahead and "BEGIN with no
// matching END" cannot be written as a single anchored pattern; pemPartialStart
// covers it and is asserted separately below.
func TestEverySecretPatternHasPartial(t *testing.T) {
	for i, p := range patterns {
		if p.partialAtEOF != nil {
			continue
		}
		if p.re.String() == pemPatternSource {
			continue
		}
		t.Errorf("pattern %d (%s) has no partialAtEOF and is not the PEM rule; "+
			"add one, or its secrets will survive truncation in plaintext",
			i, p.re.String())
	}
}

const pemPatternSource = `(?s)-----BEGIN[A-Z\s]*PRIVATE KEY-----.*?-----END[A-Z\s]*PRIVATE KEY-----`

func TestPemPartialStart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool // an unterminated key is present
	}{
		{"complete key", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", false},
		{"unterminated body", "-----BEGIN RSA PRIVATE KEY-----\nMIIEabc", true},
		{"opener cut mid marker", "text -----BEGIN RSA PRIV", true},
		{"opener cut early", "text -----BEG", true},
		{"end marker cut in half", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIV", true},
		{"no pem at all", "ordinary log output with dashes -- here", false},
		{"begin word in prose", "we -----BEGIN and later -----END fine", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pemPartialStart(tc.in) >= 0; got != tc.want {
				t.Errorf("pemPartialStart(%q) present=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPreviewPrefixKeepsOrdinaryOutput guards the other half of the contract.
// Cutting aggressively is trivially safe and useless: a preview that discards
// the whole line teaches the reader nothing. These are the output shapes with
// no whitespace to fall back on, where an over-eager rule would erase
// everything.
func TestPreviewPrefixKeepsOrdinaryOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"single line ascii", strings.Repeat("abcdefghij", 800)},
		{"compact json", `{"key":"` + strings.Repeat("v", 8000) + `"}`},
		{"base64 blob", strings.Repeat("QUJDREVGR0hJSktMTU5PUFFS", 300)},
		{"chinese without spaces", strings.Repeat("中", 2000)},
		{"go stack trace", strings.Repeat("goroutine 1 [running]:\nmain.foo(0x1)\n\t/src/a.go:42 +0x1f\n", 60)},
		{"npm warnings", strings.Repeat("npm WARN deprecated pkg@1.0.0: use bar\n", 60)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prefix, _ := PreviewPrefix(tc.in)
			// Allow a modest trailing cut; demand the bulk survives.
			if len(prefix) < len(tc.in)-64 {
				t.Errorf("kept only %d of %d bytes; an over-eager partial matcher "+
					"is erasing ordinary output", len(prefix), len(tc.in))
			}
		})
	}
}

func TestPreviewPrefixCutsAtRuneBoundary(t *testing.T) {
	// A partial matcher must never report an offset inside a multi-byte rune.
	in := strings.Repeat("中文日志", 100) + " AKIA1234"
	prefix, _ := PreviewPrefix(in)
	if !isValidUTF8(prefix) {
		t.Errorf("prefix is not valid UTF-8: %q", prefix[max(0, len(prefix)-16):])
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
