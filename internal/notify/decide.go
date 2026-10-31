package notify

import (
	"math"

	"github.com/nisaguven/artci/internal/store"
)

// UpdateThreshold: güncelleme bildirimi için en küçük büyüklük farkı (Plan §10).
const UpdateThreshold = 0.5

// Decide, bir abonenin bir olay sürümü için hangi bildirimi alacağını söyler.
// Boş dize: bildirim yok. Yorumun gerekçesi docs/NOTIFY.md'de.
//
//   - Büyüklük aboneliğin eşiğinin altındaysa: hiçbir şey.
//   - Bu abone bu olay için henüz bildirim almadıysa: "initial". Olay bu
//     abonenin eşiğini ilk kez (ilk sürümde ya da bir düzeltmeyle) geçmiştir.
//   - "initial" gönderildiyse: büyüklük kullanıcıya bildirilen değerden en az
//     0,5 farklıysa ve daha önce "update" gönderilmediyse "update".
//     Senaryo 2 (4,3 -> 4,1, fark 0,2) böylece sessiz kalır.
func Decide(st store.NotifyState, mag, minMag float64) string {
	mag, minMag = round1(mag), round1(minMag)
	if mag < minMag {
		return ""
	}
	if !st.HasInitial {
		return store.KindInitial
	}
	if st.HasUpdate {
		return "" // abone başına olay başına en fazla bir güncelleme
	}
	// Ondalık farkı tam sayıya çevirerek karşılaştır: 4.6-4.1 = 0.4999... sorunu olmasın.
	if math.Abs(math.Round(mag*10)-math.Round(st.LastMag*10)) >= UpdateThreshold*10 {
		return store.KindUpdate
	}
	return ""
}
