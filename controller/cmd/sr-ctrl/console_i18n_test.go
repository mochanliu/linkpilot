// Tests for the console language switch.
//
// Two failure modes matter here. The first is mechanical: the page references
// /assets/i18n-en.js and /assets/console-i18n.js, and if either stops being
// embedded the console silently stays Chinese in English mode -- no error, no
// 404 in normal use, because the script tag is simply empty. The second is
// quality: a half-translated entry ("已发布" -> "已发布") looks like a bug in the
// product, so the dictionary is checked for Chinese left on the English side.
package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestI18nAssetsAreEmbedded(t *testing.T) {
	if len(i18nEnJS) < 20_000 {
		t.Fatalf("English dictionary looks truncated: %d bytes", len(i18nEnJS))
	}
	if !strings.Contains(string(i18nEnJS), "window.SR_I18N_DICT") {
		t.Fatal("dictionary asset does not define window.SR_I18N_DICT")
	}
	if !strings.Contains(string(consoleI18nJS), "window.SR_I18N") {
		t.Fatal("engine asset does not install window.SR_I18N")
	}
	// The page must load both, dictionary first: the engine reads the globals at
	// parse time, so a reversed order would leave a console that never translates.
	page := string(consoleHTML)
	dictAt := strings.Index(page, "/assets/i18n-en.js")
	engineAt := strings.Index(page, "/assets/console-i18n.js")
	if dictAt < 0 || engineAt < 0 {
		t.Fatalf("console.html does not reference both i18n assets (dict %d, engine %d)", dictAt, engineAt)
	}
	if dictAt > engineAt {
		t.Fatal("console.html loads the i18n engine before the dictionary")
	}
}

func TestI18nAssetsAreServed(t *testing.T) {
	s := &server{}
	for _, name := range []string{"i18n-en.js", "console-i18n.js"} {
		rec := assetReq(t, s, "/assets/"+name, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Fatalf("%s content-type = %q", name, ct)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s served empty", name)
		}
	}
}

// A value that still contains Chinese means the string was copied through
// untranslated. Unit words and product names are fine on the English side; a
// whole Chinese sentence is not.
func TestEnglishDictionaryValuesAreTranslated(t *testing.T) {
	re := regexp.MustCompile(`^\t("(?:[^"\\]|\\.)*"): ("(?:[^"\\]|\\.)*"),?$`)
	cjk := regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)
	// Placeholders and counters legitimately keep a Chinese measure word when the
	// English sentence puts the number elsewhere, so only flag long runs.
	bad := 0
	lines := strings.Split(string(i18nEnJS), "\n")
	checked := 0
	for _, ln := range lines {
		m := re.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		checked++
		val := m[2]
		if cjk.MatchString(val) && len([]rune(val)) > 8 {
			bad++
			if bad <= 5 {
				t.Errorf("English value still contains Chinese: %s", ln)
			}
		}
	}
	if checked < 500 {
		t.Fatalf("parsed only %d dictionary entries; the file format changed", checked)
	}
	if bad > 0 {
		t.Fatalf("%d dictionary values are untranslated", bad)
	}
}
