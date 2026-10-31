package web

import (
	"io/fs"
	"strings"
	"testing"
)

// Gömülü sitenin vazgeçilmezlerini korur: uyarı bandı, CDN bütünlük
// özetleri ve kaynak metinlerinin innerHTML ile yazılmaması.
func TestEmbeddedSite(t *testing.T) {
	read := func(name string) string {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	html := read("index.html")
	for _, want := range []string{
		"Artçı bir eğitim projesidir; resmî bir uyarı sistemi değildir.",
		`integrity="sha256-`,
		`aria-live="polite"`,
		`<html lang="tr">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html içinde yok: %q", want)
		}
	}
	js := read("app.js")
	if strings.Contains(js, "innerHTML") || strings.Contains(js, "insertAdjacentHTML") {
		t.Error("app.js HTML enjekte etmemeli; textContent kullanın")
	}
	if !strings.Contains(js, "Europe/Istanbul") {
		t.Error("saatler Türkiye saatiyle gösterilmeli")
	}
	read("style.css")
}
