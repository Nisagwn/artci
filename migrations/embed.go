// Package migrations, SQL dosyalarını ikili dosyaya gömer.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
