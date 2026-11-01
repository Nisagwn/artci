package replay

import (
	"strings"
	"testing"
	"time"
)

// Gerçek ISC yanıtından kısaltılmış tek olay: ISK ve AFAD çözümleri, ISC'nin
// kendi çözümü (atlanmalı) ve ISK'nın iki büyüklüğü (Mw seçilmeli).
const iscSample = `<?xml version="1.0" encoding="UTF-8"?>
<q:quakeml xmlns:q="http://quakeml.org/xmlns/quakeml/1.2" xmlns="http://quakeml.org/xmlns/bed/1.2">
  <eventParameters publicID="smi:ISC/bulletin">
    <event publicID="smi:ISC/evid=644684632">
      <origin publicID="smi:ISC/origid=638845291">
        <time><value>2025-08-10T15:06:17.40Z</value></time>
        <latitude><value>39.2173</value></latitude>
        <longitude><value>28.0740</value></longitude>
        <depth><value>13500.0</value></depth>
        <creationInfo><author>ISK</author><agencyID>ISK</agencyID></creationInfo>
      </origin>
      <origin publicID="smi:ISC/origid=640053357">
        <time><value>2025-08-10T15:06:18.00Z</value></time>
        <latitude><value>39.2360</value></latitude>
        <longitude><value>28.0630</value></longitude>
        <depth><value>7000.0</value></depth>
        <creationInfo><author>AFAD</author><agencyID>AFAD</agencyID></creationInfo>
      </origin>
      <origin publicID="smi:ISC/origid=1">
        <time><value>2025-08-10T15:06:17.90Z</value></time>
        <latitude><value>39.22</value></latitude>
        <longitude><value>28.07</value></longitude>
        <creationInfo><agencyID>ISC</agencyID></creationInfo>
      </origin>
      <magnitude publicID="smi:ISC/magid=647782233">
        <mag><value>3.20</value></mag><type>ML</type>
        <originID>smi:ISC/origid=638845291</originID>
      </magnitude>
      <magnitude publicID="smi:ISC/magid=647782232">
        <mag><value>3.30</value></mag><type>Mw</type>
        <originID>smi:ISC/origid=638845291</originID>
      </magnitude>
      <magnitude publicID="smi:ISC/magid=649333479">
        <mag><value>3.10</value></mag><type>ML</type>
        <originID>smi:ISC/origid=640053357</originID>
      </magnitude>
    </event>
  </eventParameters>
</q:quakeml>`

func TestParseISC(t *testing.T) {
	obs, err := ParseISC(strings.NewReader(iscSample))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("%d gözlem, beklenen 2 (ISC çözümü atlanmalı)", len(obs))
	}
	k, a := obs[0], obs[1]
	if k.Truth != "644684632" || a.Truth != k.Truth {
		t.Errorf("truth: %q %q", k.Truth, a.Truth)
	}
	if k.Report.Source != "kandilli" || k.Report.Magnitude != 3.3 || k.Report.MagType != "Mw" {
		t.Errorf("kandilli: %+v (Mw tercih edilmeli)", k.Report)
	}
	if a.Report.Source != "afad" || a.Report.Magnitude != 3.1 || *a.Report.DepthKm != 7 {
		t.Errorf("afad: %+v", a.Report)
	}
	if want := time.Date(2025, 8, 10, 15, 6, 17, 400e6, time.UTC); !k.Report.OriginTime.Equal(want) {
		t.Errorf("zaman %v", k.Report.OriginTime)
	}
}

func TestParseISC_Empty(t *testing.T) {
	obs, err := ParseISC(strings.NewReader("")) // HTTP 204
	if err != nil || obs != nil {
		t.Fatalf("%v %v", obs, err)
	}
}

func TestCatalogFromObservations_Deterministic(t *testing.T) {
	obs, _ := ParseISC(strings.NewReader(iscSample))
	a := CatalogFromObservations(obs, "x", 1)
	b := CatalogFromObservations(obs, "x", 1)
	for i := range a {
		if a[i].ArriveS != b[i].ArriveS || a[i].ArriveS <= 0 {
			t.Fatalf("aynı tohum aynı varışları vermeli: %v %v", a[i].ArriveS, b[i].ArriveS)
		}
	}
}
