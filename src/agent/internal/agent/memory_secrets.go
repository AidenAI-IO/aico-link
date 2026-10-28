package agent

import (
	"strings"
	"unicode"
)

// memorySensitiveKind classifies a secret in a memory. Long-term memory is
// plain files that are recalled into later prompts and backups, so a secret
// written there outlives the moment it was needed. save_memory stores one only
// when the user explicitly asked, and marks it so recall and the user profile
// keep it out of view.
//
// The model declares the kind through save_memory's sensitive argument; no
// keyword list or number pattern could recognize secrets reliably in every
// language. Code enforces what follows from the declaration.
type memorySensitiveKind string

const (
	memorySensitiveNone       memorySensitiveKind = ""
	memorySensitiveCredential memorySensitiveKind = "credential"
	memorySensitiveCard       memorySensitiveKind = "payment_card"
)

// parseMemorySensitiveKind normalizes the model's sensitive argument. Any
// value other than none or a known kind is treated as a credential, so an
// unexpected spelling errs toward protection.
func parseMemorySensitiveKind(value string) memorySensitiveKind {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "false", "no", "0":
		return memorySensitiveNone
	case string(memorySensitiveCard):
		return memorySensitiveCard
	default:
		return memorySensitiveCredential
	}
}

// userRequestQuoted reports whether quote, the user's request to remember as
// the model cited it, really appears in this run's user texts. Comparing
// letters and digits only, case-insensitively, lets a faithful quote match in
// any language without a keyword list. The quote must contain at least two
// letters so that citing the secret value alone does not count as a request.
// Whether the quoted words are a request stays the model's judgment; this
// check only stops it from inventing one.
func userRequestQuoted(quote string, userTexts ...string) bool {
	normalized := normalizeQuotedText(quote)
	letters := 0
	for _, r := range normalized {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 2 {
		return false
	}
	for _, text := range userTexts {
		if strings.Contains(normalizeQuotedText(text), normalized) {
			return true
		}
	}
	return false
}

func normalizeQuotedText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
}
