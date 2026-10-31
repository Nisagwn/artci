package geo

import (
	"math"
	"testing"
)

func TestHaversineKm(t *testing.T) {
	tests := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want, tol              float64
	}{
		{"aynı nokta", 39.0, 28.0, 39.0, 28.0, 0, 0.001},
		{"İstanbul-Ankara", 41.0082, 28.9784, 39.9334, 32.8597, 350, 5},
		{"1 derece enlem", 39.0, 28.0, 40.0, 28.0, 111.2, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HaversineKm(tt.lat1, tt.lon1, tt.lat2, tt.lon2)
			if math.Abs(got-tt.want) > tt.tol {
				t.Fatalf("got %.2f, want %.2f±%.2f", got, tt.want, tt.tol)
			}
		})
	}
}

func TestCitiesComplete(t *testing.T) {
	if len(Cities) != 81 {
		t.Fatalf("81 il bekleniyordu, %d var", len(Cities))
	}
}

func TestFindCity(t *testing.T) {
	for _, in := range []string{"İzmir", "izmir", "IZMIR", "Izmir", " balıkesir ", "Balikesir", "urfa", "Kahramanmaraş"} {
		if _, ok := FindCity(in); !ok {
			t.Errorf("%q bulunamadı", in)
		}
	}
	if _, ok := FindCity("Atlantis"); ok {
		t.Error("olmayan şehir bulundu")
	}
}

func TestNearestCity(t *testing.T) {
	// Sındırgı (Balıkesir) civarı
	c, d := NearestCity(39.21, 28.15, 0)
	if c.Name != "Balıkesir" && c.Name != "Manisa" {
		t.Fatalf("beklenmeyen il: %s", c.Name)
	}
	if d > 100 {
		t.Fatalf("mesafe çok büyük: %.1f", d)
	}
}
