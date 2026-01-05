package main

import (
	"fmt"
	"strings"
)

// Helpers per costruire key S3 stabili e sicure, evitando caratteri che
// possono creare path indesiderati.
func sanitizeKeyPart(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	if s == "" {
		return "_"
	}
	return s
}

// Chiavi S3 per export Tolgee (traduzioni).
func s3LatestKey(appID, lang string) string {
	return fmt.Sprintf("localizations/%s/%s/latest.json", sanitizeKeyPart(appID), sanitizeKeyPart(lang))
}

func s3VersionKey(appID, lang, tsUTC, sha string) string {
	return fmt.Sprintf("localizations/%s/%s/%s_%s.json", sanitizeKeyPart(appID), sanitizeKeyPart(lang), tsUTC, sha)
}
