// Package web, canlı haritanın statik dosyalarını ikili dosyaya gömer;
// böylece api tek bir dosya olarak dağıtılır, ayrı bir web sunucusu gerekmez.
package web

import "embed"

//go:embed index.html app.js style.css
var FS embed.FS
