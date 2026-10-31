package match

import (
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

func TestReportChanged(t *testing.T) {
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.3)
	b := a
	b.ReceivedAt = a.ReceivedAt.Add(time.Hour) // polling'in yeniden getirmesi
	b.Raw = []byte(`{"fark":"var"}`)
	b.DepthKm = fptr(10.04) // NUMERIC(5,1) ile aynı değer
	if ReportChanged(a, b) {
		t.Fatal("ReceivedAt/Raw/yuvarlama farkı değişiklik sayılmamalı")
	}
	b.Magnitude = 4.1
	if !ReportChanged(a, b) {
		t.Fatal("büyüklük değişikliği görülmeli")
	}
	c := a
	c.DepthKm = nil
	if !ReportChanged(a, c) {
		t.Fatal("derinliğin kaybolması değişikliktir")
	}
}

func TestEventChanged(t *testing.T) {
	a := Resolve([]normalize.Report{rep("afad", "a1", 0, 39.21, 28.15, 4.1)})
	b := a
	b.OriginTime = a.OriginTime.Add(300 * time.Nanosecond) // DB gidiş-dönüşü
	if EventChanged(a, b) {
		t.Fatal("mikro saniye altı fark değişiklik değildir")
	}
	b.Sources = []string{"afad", "emsc"}
	if !EventChanged(a, b) {
		t.Fatal("kaynak eklenmesi değişikliktir")
	}
}
