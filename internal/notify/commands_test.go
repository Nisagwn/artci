package notify

import (
	"errors"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		city     string
		radius   int
		minMag   float64
		wantErr  bool
	}{
		{in: "/start", wantName: CmdStart},
		{in: "/yardim", wantName: CmdHelp},
		{in: "/yardım", wantName: CmdHelp},
		{in: "/help", wantName: CmdHelp},
		{in: "/aboneliklerim", wantName: CmdSubscriptions},
		{in: "/iptal", wantName: CmdCancel},
		{in: "/iptal@ArtciBot", wantName: CmdCancel},
		{in: "/abone Balıkesir 100 3.5", wantName: CmdSubscribe, city: "Balıkesir", radius: 100, minMag: 3.5},
		{in: "/abone balikesir 50 3,5", wantName: CmdSubscribe, city: "Balıkesir", radius: 50, minMag: 3.5},
		{in: "/abone@ArtciBot İzmir 200 4", wantName: CmdSubscribe, city: "İzmir", radius: 200, minMag: 4},
		{in: "/abone Balıkesir", wantName: CmdSubscribe, city: "Balıkesir", radius: DefaultRadiusKm, minMag: DefaultMinMag},
		{in: "/abone Balıkesir 150", wantName: CmdSubscribe, city: "Balıkesir", radius: 150, minMag: DefaultMinMag},
		{in: "/abone Balıkesir 150km 4,25", wantName: CmdSubscribe, city: "Balıkesir", radius: 150, minMag: 4.3},
		{in: "/abone Afyon 100 3.5", wantName: CmdSubscribe, city: "Afyonkarahisar", radius: 100, minMag: 3.5},
		{in: "/abone   KAHRAMANMARAŞ   10   9", wantName: CmdSubscribe, city: "Kahramanmaraş", radius: 10, minMag: 9},
		{in: "/ABONE Van 500 2", wantName: CmdSubscribe, city: "Van", radius: 500, minMag: 2},
		// Hatalar
		{in: "/abone", wantErr: true},
		{in: "/abone Atlantis 100 3.5", wantErr: true},
		{in: "/abone Balıkesir 5 3.5", wantErr: true},
		{in: "/abone Balıkesir 501 3.5", wantErr: true},
		{in: "/abone Balıkesir 100,5 3.5", wantErr: true},
		{in: "/abone Balıkesir 100 1,5", wantErr: true},
		{in: "/abone Balıkesir 100 9,5", wantErr: true},
		{in: "/abone Balıkesir 100 3x", wantErr: true},
		{in: "/abone 100 Balıkesir", wantErr: true},
		{in: "/abone Balıkesir 100 3.5 7", wantErr: true}, // fazla sayı -> şehir "Balıkesir 100" olur
		{in: "/bilinmeyen", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCommand(tt.in)
			if tt.wantErr {
				var ue *UserError
				if !errors.As(err, &ue) {
					t.Fatalf("UserError bekleniyordu, got cmd=%+v err=%v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}
			if got.Name != tt.wantName {
				t.Fatalf("name = %q, want %q", got.Name, tt.wantName)
			}
			if tt.wantName != CmdSubscribe {
				return
			}
			if got.City.Name != tt.city || got.RadiusKm != tt.radius || got.MinMag != tt.minMag {
				t.Fatalf("got %s/%d/%v, want %s/%d/%v", got.City.Name, got.RadiusKm, got.MinMag, tt.city, tt.radius, tt.minMag)
			}
		})
	}
}

func TestParseCommandNotCommand(t *testing.T) {
	for _, in := range []string{"", "   ", "merhaba", "abone Balıkesir"} {
		if _, err := ParseCommand(in); !errors.Is(err, ErrNotCommand) {
			t.Errorf("%q: ErrNotCommand bekleniyordu, got %v", in, err)
		}
	}
}

func TestFormatDecimal(t *testing.T) {
	for in, want := range map[float64]string{4.1: "4,1", 3.5: "3,5", 5: "5,0", 4.26: "4,3"} {
		if got := FormatDecimal(in); got != want {
			t.Errorf("FormatDecimal(%v) = %q, want %q", in, got, want)
		}
	}
	if got := formatTrimmed(10.0); got != "10" {
		t.Errorf("formatTrimmed(10) = %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	if got := EscapeHTML(`<b>A & B</b>`); got != "&lt;b&gt;A &amp; B&lt;/b&gt;" {
		t.Fatalf("got %q", got)
	}
}
