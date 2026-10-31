// Package geo, mesafe hesabı ve Türkiye il merkezleri veri setidir.
// Abonelikler (notifier) ve "en yakın büyük şehir" (summarizer) bu paketi kullanır.
// Koordinatlar il merkezlerinin yaklaşık konumudur (kişisel konum değil).
package geo

import (
	"math"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const earthRadiusKm = 6371.0088

// HaversineKm, iki nokta arasındaki büyük daire mesafesini km olarak döner.
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(a)))
}

// City, bir il merkezidir.
type City struct {
	Name       string
	Lat, Lon   float64
	Population int // yaklaşık il nüfusu (bin); "büyük şehir" seçimi için
}

// Cities: 81 il merkezi.
var Cities = []City{
	{"Adana", 37.0000, 35.3213, 2270}, {"Adıyaman", 37.7648, 38.2786, 635},
	{"Afyonkarahisar", 38.7507, 30.5567, 750}, {"Ağrı", 39.7191, 43.0503, 510},
	{"Amasya", 40.6499, 35.8353, 335}, {"Ankara", 39.9334, 32.8597, 5800},
	{"Antalya", 36.8969, 30.7133, 2700}, {"Artvin", 41.1828, 41.8183, 170},
	{"Aydın", 37.8560, 27.8416, 1150}, {"Balıkesir", 39.6484, 27.8826, 1260},
	{"Bilecik", 40.1506, 29.9792, 230}, {"Bingöl", 38.8847, 40.4939, 285},
	{"Bitlis", 38.4006, 42.1095, 355}, {"Bolu", 40.7395, 31.6116, 320},
	{"Burdur", 37.7203, 30.2908, 275}, {"Bursa", 40.1885, 29.0610, 3200},
	{"Çanakkale", 40.1553, 26.4142, 560}, {"Çankırı", 40.6013, 33.6134, 200},
	{"Çorum", 40.5506, 34.9556, 525}, {"Denizli", 37.7765, 29.0864, 1060},
	{"Diyarbakır", 37.9144, 40.2306, 1800}, {"Edirne", 41.6818, 26.5623, 415},
	{"Elazığ", 38.6810, 39.2264, 600}, {"Erzincan", 39.7500, 39.5000, 240},
	{"Erzurum", 39.9000, 41.2700, 750}, {"Eskişehir", 39.7767, 30.5206, 900},
	{"Gaziantep", 37.0662, 37.3833, 2150}, {"Giresun", 40.9128, 38.3895, 450},
	{"Gümüşhane", 40.4386, 39.5086, 145}, {"Hakkari", 37.5833, 43.7333, 275},
	{"Hatay", 36.2028, 36.1606, 1550}, {"Isparta", 37.7648, 30.5566, 445},
	{"Mersin", 36.8000, 34.6333, 1900}, {"İstanbul", 41.0082, 28.9784, 15700},
	{"İzmir", 38.4237, 27.1428, 4450}, {"Kars", 40.6013, 43.0975, 275},
	{"Kastamonu", 41.3887, 33.7827, 380}, {"Kayseri", 38.7312, 35.4787, 1450},
	{"Kırklareli", 41.7333, 27.2167, 370}, {"Kırşehir", 39.1425, 34.1709, 245},
	{"Kocaeli", 40.8533, 29.8815, 2100}, {"Konya", 37.8746, 32.4932, 2300},
	{"Kütahya", 39.4167, 29.9833, 580}, {"Malatya", 38.3552, 38.3095, 800},
	{"Manisa", 38.6191, 27.4289, 1470}, {"Kahramanmaraş", 37.5858, 36.9371, 1150},
	{"Mardin", 37.3212, 40.7245, 870}, {"Muğla", 37.2153, 28.3636, 1050},
	{"Muş", 38.9462, 41.7539, 400}, {"Nevşehir", 38.6939, 34.6857, 310},
	{"Niğde", 37.9667, 34.6833, 365}, {"Ordu", 40.9839, 37.8764, 760},
	{"Rize", 41.0201, 40.5234, 345}, {"Sakarya", 40.7569, 30.3783, 1100},
	{"Samsun", 41.2928, 36.3313, 1370}, {"Siirt", 37.9333, 41.9500, 330},
	{"Sinop", 42.0231, 35.1531, 220}, {"Sivas", 39.7477, 37.0179, 640},
	{"Tekirdağ", 40.9833, 27.5167, 1150}, {"Tokat", 40.3167, 36.5500, 600},
	{"Trabzon", 41.0015, 39.7178, 820}, {"Tunceli", 39.1079, 39.5401, 85},
	{"Şanlıurfa", 37.1591, 38.7969, 2170}, {"Uşak", 38.6823, 29.4082, 375},
	{"Van", 38.4891, 43.4089, 1120}, {"Yozgat", 39.8181, 34.8147, 420},
	{"Zonguldak", 41.4564, 31.7987, 590}, {"Aksaray", 38.3687, 34.0370, 435},
	{"Bayburt", 40.2552, 40.2249, 85}, {"Karaman", 37.1759, 33.2287, 260},
	{"Kırıkkale", 39.8468, 33.5153, 280}, {"Batman", 37.8812, 41.1351, 630},
	{"Şırnak", 37.5164, 42.4611, 560}, {"Bartın", 41.6344, 32.3375, 205},
	{"Ardahan", 41.1105, 42.7022, 95}, {"Iğdır", 39.9237, 44.0450, 205},
	{"Yalova", 40.6500, 29.2667, 300}, {"Karabük", 41.2061, 32.6204, 250},
	{"Kilis", 36.7184, 37.1212, 150}, {"Osmaniye", 37.0742, 36.2478, 560},
	{"Düzce", 40.8438, 31.1565, 410},
}

var trLower = cases.Lower(language.Turkish)

// fold, karşılaştırma için Türkçe küçük harfe çevirir ve aksanları sadeleştirir
// (ör. "İzmir", "izmir", "IZMIR" ve "Izmir" eşleşsin).
func fold(s string) string {
	s = trLower.String(strings.TrimSpace(s))
	r := strings.NewReplacer("ı", "i", "ğ", "g", "ü", "u", "ş", "s", "ö", "o", "ç", "c", "â", "a", "î", "i", "û", "u")
	return r.Replace(s)
}

var byName = func() map[string]City {
	m := make(map[string]City, len(Cities))
	for _, c := range Cities {
		m[fold(c.Name)] = c
	}
	m["icel"] = m["mersin"]
	m["antep"] = m["gaziantep"]
	m["urfa"] = m["sanliurfa"]
	m["maras"] = m["kahramanmaras"]
	m["afyon"] = m["afyonkarahisar"]
	return m
}()

// FindCity, il adını (büyük/küçük harf ve Türkçe karakterden bağımsız) arar.
func FindCity(name string) (City, bool) {
	c, ok := byName[fold(name)]
	return c, ok
}

// NearestCity, verilen noktaya en yakın il merkezini ve uzaklığını döner.
// minPopulation > 0 ise yalnızca o nüfusun (bin) üstündeki iller dikkate alınır.
func NearestCity(lat, lon float64, minPopulation int) (City, float64) {
	best, bestD := City{}, math.Inf(1)
	for _, c := range Cities {
		if c.Population < minPopulation {
			continue
		}
		if d := HaversineKm(lat, lon, c.Lat, c.Lon); d < bestD {
			best, bestD = c, d
		}
	}
	return best, bestD
}

// CityNames, alfabetik il adları listesidir (bot yardım mesajı için).
func CityNames() []string {
	out := make([]string, len(Cities))
	for i, c := range Cities {
		out[i] = c.Name
	}
	sort.Strings(out)
	return out
}
